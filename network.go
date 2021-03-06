package docker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/dotcloud/docker/iptables"
	"github.com/dotcloud/docker/netlink"
	"github.com/dotcloud/docker/proxy"
	"github.com/dotcloud/docker/utils"
	"log"
	"net"
	"strconv"
	"sync"
)

const (
	DefaultNetworkBridge = "docker0"
	DisableNetworkBridge = "none"
	portRangeStart       = 49153
	portRangeEnd         = 65535
)

// Calculates the first and last IP addresses in an IPNet
func networkRange(network *net.IPNet) (net.IP, net.IP) {
	netIP := network.IP.To4()
	firstIP := netIP.Mask(network.Mask)
	lastIP := net.IPv4(0, 0, 0, 0).To4()
	for i := 0; i < len(lastIP); i++ {
		lastIP[i] = netIP[i] | ^network.Mask[i]
	}
	return firstIP, lastIP
}

// Detects overlap between one IPNet and another
func networkOverlaps(netX *net.IPNet, netY *net.IPNet) bool {
	firstIP, _ := networkRange(netX)
	if netY.Contains(firstIP) {
		return true
	}
	firstIP, _ = networkRange(netY)
	if netX.Contains(firstIP) {
		return true
	}
	return false
}

// Converts a 4 bytes IP into a 32 bit integer
func ipToInt(ip net.IP) int32 {
	return int32(binary.BigEndian.Uint32(ip.To4()))
}

// Converts 32 bit integer into a 4 bytes IP address
func intToIP(n int32) net.IP {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(n))
	return net.IP(b)
}

// Given a netmask, calculates the number of available hosts
func networkSize(mask net.IPMask) int32 {
	m := net.IPv4Mask(0, 0, 0, 0)
	for i := 0; i < net.IPv4len; i++ {
		m[i] = ^mask[i]
	}

	return int32(binary.BigEndian.Uint32(m)) + 1
}

func checkRouteOverlaps(networks []*net.IPNet, dockerNetwork *net.IPNet) error {
	for _, network := range networks {
		if networkOverlaps(dockerNetwork, network) {
			return fmt.Errorf("Network %s is already routed: '%s'", dockerNetwork, network)
		}
	}
	return nil
}

func checkNameserverOverlaps(nameservers []string, dockerNetwork *net.IPNet) error {
	if len(nameservers) > 0 {
		for _, ns := range nameservers {
			_, nsNetwork, err := net.ParseCIDR(ns)
			if err != nil {
				return err
			}
			if networkOverlaps(dockerNetwork, nsNetwork) {
				return fmt.Errorf("%s overlaps nameserver %s", dockerNetwork, nsNetwork)
			}
		}
	}
	return nil
}

// CreateBridgeIface creates a network bridge interface on the host system with the name `ifaceName`,
// and attempts to configure it with an address which doesn't conflict with any other interface on the host.
// If it can't find an address which doesn't conflict, it will return an error.
func CreateBridgeIface(config *DaemonConfig) error {
	addrs := []string{
		// Here we don't follow the convention of using the 1st IP of the range for the gateway.
		// This is to use the same gateway IPs as the /24 ranges, which predate the /16 ranges.
		// In theory this shouldn't matter - in practice there's bound to be a few scripts relying
		// on the internal addressing or other stupid things like that.
		// The shouldn't, but hey, let's not break them unless we really have to.
		"172.17.42.1/16", // Don't use 172.16.0.0/16, it conflicts with EC2 DNS 172.16.0.23
		"10.0.42.1/16",   // Don't even try using the entire /8, that's too intrusive
		"10.1.42.1/16",
		"10.42.42.1/16",
		"172.16.42.1/24",
		"172.16.43.1/24",
		"172.16.44.1/24",
		"10.0.42.1/24",
		"10.0.43.1/24",
		"192.168.42.1/24",
		"192.168.43.1/24",
		"192.168.44.1/24",
	}

	nameservers := []string{}
	resolvConf, _ := utils.GetResolvConf()
	// we don't check for an error here, because we don't really care
	// if we can't read /etc/resolv.conf. So instead we skip the append
	// if resolvConf is nil. It either doesn't exist, or we can't read it
	// for some reason.
	if resolvConf != nil {
		nameservers = append(nameservers, utils.GetNameserversAsCIDR(resolvConf)...)
	}

	var ifaceAddr string
	for _, addr := range addrs {
		_, dockerNetwork, err := net.ParseCIDR(addr)
		if err != nil {
			return err
		}
		routes, err := netlink.NetworkGetRoutes()
		if err != nil {
			return err
		}
		if err := checkRouteOverlaps(routes, dockerNetwork); err == nil {
			if err := checkNameserverOverlaps(nameservers, dockerNetwork); err == nil {
				ifaceAddr = addr
				break
			}
		} else {
			utils.Debugf("%s: %s", addr, err)
		}
	}
	if ifaceAddr == "" {
		return fmt.Errorf("Could not find a free IP address range for interface '%s'. Please configure its address manually and run 'docker -b %s'", config.BridgeIface, config.BridgeIface)
	}
	utils.Debugf("Creating bridge %s with network %s", config.BridgeIface, ifaceAddr)

	if err := netlink.NetworkLinkAdd(config.BridgeIface, "bridge"); err != nil {
		return fmt.Errorf("Error creating bridge: %s", err)
	}
	iface, err := net.InterfaceByName(config.BridgeIface)
	if err != nil {
		return err
	}
	ipAddr, ipNet, err := net.ParseCIDR(ifaceAddr)
	if err != nil {
		return err
	}
	if netlink.NetworkLinkAddIp(iface, ipAddr, ipNet); err != nil {
		return fmt.Errorf("Unable to add private network: %s", err)
	}
	if err := netlink.NetworkLinkUp(iface); err != nil {
		return fmt.Errorf("Unable to start network bridge: %s", err)
	}

	if config.EnableIptables {
		if output, err := iptables.Raw("-t", "nat", "-A", "POSTROUTING", "-s", ifaceAddr,
			"!", "-d", ifaceAddr, "-j", "MASQUERADE"); err != nil {
			return fmt.Errorf("Unable to enable network bridge NAT: %s", err)
		} else if len(output) != 0 {
			return fmt.Errorf("Error iptables postrouting: %s", output)
		}
	}
	return nil
}

// Return the IPv4 address of a network interface
func getIfaceAddr(name string) (net.Addr, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	var addrs4 []net.Addr
	for _, addr := range addrs {
		ip := (addr.(*net.IPNet)).IP
		if ip4 := ip.To4(); len(ip4) == net.IPv4len {
			addrs4 = append(addrs4, addr)
		}
	}
	switch {
	case len(addrs4) == 0:
		return nil, fmt.Errorf("Interface %v has no IP addresses", name)
	case len(addrs4) > 1:
		fmt.Printf("Interface %v has more than 1 IPv4 address. Defaulting to using %v\n",
			name, (addrs4[0].(*net.IPNet)).IP)
	}
	return addrs4[0], nil
}

// Port mapper takes care of mapping external ports to containers by setting
// up iptables rules.
// It keeps track of all mappings and is able to unmap at will
type PortMapper struct {
	tcpMapping map[int]*net.TCPAddr
	tcpProxies map[int]proxy.Proxy
	udpMapping map[int]*net.UDPAddr
	udpProxies map[int]proxy.Proxy

	iptables  *iptables.Chain
	defaultIp net.IP
}

func (mapper *PortMapper) Map(ip net.IP, port int, backendAddr net.Addr) error {
	if _, isTCP := backendAddr.(*net.TCPAddr); isTCP {
		backendPort := backendAddr.(*net.TCPAddr).Port
		backendIP := backendAddr.(*net.TCPAddr).IP
		if mapper.iptables != nil {
			if err := mapper.iptables.Forward(iptables.Add, ip, port, "tcp", backendIP.String(), backendPort); err != nil {
				return err
			}
		}
		mapper.tcpMapping[port] = backendAddr.(*net.TCPAddr)
		proxy, err := proxy.NewProxy(&net.TCPAddr{IP: ip, Port: port}, backendAddr)
		if err != nil {
			mapper.Unmap(ip, port, "tcp")
			return err
		}
		mapper.tcpProxies[port] = proxy
		go proxy.Run()
	} else {
		backendPort := backendAddr.(*net.UDPAddr).Port
		backendIP := backendAddr.(*net.UDPAddr).IP
		if mapper.iptables != nil {
			if err := mapper.iptables.Forward(iptables.Add, ip, port, "udp", backendIP.String(), backendPort); err != nil {
				return err
			}
		}
		mapper.udpMapping[port] = backendAddr.(*net.UDPAddr)
		proxy, err := proxy.NewProxy(&net.UDPAddr{IP: ip, Port: port}, backendAddr)
		if err != nil {
			mapper.Unmap(ip, port, "udp")
			return err
		}
		mapper.udpProxies[port] = proxy
		go proxy.Run()
	}
	return nil
}

func (mapper *PortMapper) Unmap(ip net.IP, port int, proto string) error {
	if proto == "tcp" {
		backendAddr, ok := mapper.tcpMapping[port]
		if !ok {
			return fmt.Errorf("Port tcp/%v is not mapped", port)
		}
		if proxy, exists := mapper.tcpProxies[port]; exists {
			proxy.Close()
			delete(mapper.tcpProxies, port)
		}
		if mapper.iptables != nil {
			if err := mapper.iptables.Forward(iptables.Delete, ip, port, proto, backendAddr.IP.String(), backendAddr.Port); err != nil {
				return err
			}
		}
		delete(mapper.tcpMapping, port)
	} else {
		backendAddr, ok := mapper.udpMapping[port]
		if !ok {
			return fmt.Errorf("Port udp/%v is not mapped", port)
		}
		if proxy, exists := mapper.udpProxies[port]; exists {
			proxy.Close()
			delete(mapper.udpProxies, port)
		}
		if mapper.iptables != nil {
			if err := mapper.iptables.Forward(iptables.Delete, ip, port, proto, backendAddr.IP.String(), backendAddr.Port); err != nil {
				return err
			}
		}
		delete(mapper.udpMapping, port)
	}
	return nil
}

func newPortMapper(config *DaemonConfig) (*PortMapper, error) {
	// We can always try removing the iptables
	if err := iptables.RemoveExistingChain("DOCKER"); err != nil {
		return nil, err
	}
	var chain *iptables.Chain
	if config.EnableIptables {
		var err error
		chain, err = iptables.NewChain("DOCKER", config.BridgeIface)
		if err != nil {
			return nil, fmt.Errorf("Failed to create DOCKER chain: %s", err)
		}
	}

	mapper := &PortMapper{
		tcpMapping: make(map[int]*net.TCPAddr),
		tcpProxies: make(map[int]proxy.Proxy),
		udpMapping: make(map[int]*net.UDPAddr),
		udpProxies: make(map[int]proxy.Proxy),
		iptables:   chain,
		defaultIp:  config.DefaultIp,
	}
	return mapper, nil
}

// Port allocator: Automatically allocate and release networking ports
type PortAllocator struct {
	sync.Mutex
	inUse    map[int]struct{}
	fountain chan int
	quit     chan bool
}

func (alloc *PortAllocator) runFountain() {
	for {
		for port := portRangeStart; port < portRangeEnd; port++ {
			select {
			case alloc.fountain <- port:
			case quit := <-alloc.quit:
				if quit {
					return
				}
			}
		}
	}
}

// FIXME: Release can no longer fail, change its prototype to reflect that.
func (alloc *PortAllocator) Release(port int) error {
	utils.Debugf("Releasing %d", port)
	alloc.Lock()
	delete(alloc.inUse, port)
	alloc.Unlock()
	return nil
}

func (alloc *PortAllocator) Acquire(port int) (int, error) {
	utils.Debugf("Acquiring %d", port)
	if port == 0 {
		// Allocate a port from the fountain
		for port := range alloc.fountain {
			if _, err := alloc.Acquire(port); err == nil {
				return port, nil
			}
		}
		return -1, fmt.Errorf("Port generator ended unexpectedly")
	}
	alloc.Lock()
	defer alloc.Unlock()
	if _, inUse := alloc.inUse[port]; inUse {
		return -1, fmt.Errorf("Port already in use: %d", port)
	}
	alloc.inUse[port] = struct{}{}
	return port, nil
}

func (alloc *PortAllocator) Close() error {
	alloc.quit <- true
	close(alloc.quit)
	close(alloc.fountain)
	return nil
}

func newPortAllocator() (*PortAllocator, error) {
	allocator := &PortAllocator{
		inUse:    make(map[int]struct{}),
		fountain: make(chan int),
		quit:     make(chan bool),
	}
	go allocator.runFountain()
	return allocator, nil
}

// IP allocator: Automatically allocate and release networking ports
type IPAllocator struct {
	network       *net.IPNet
	queueAlloc    chan allocatedIP
	queueReleased chan net.IP
	inUse         map[int32]struct{}
	quit          chan bool
}

type allocatedIP struct {
	ip  net.IP
	err error
}

func (alloc *IPAllocator) run() {
	firstIP, _ := networkRange(alloc.network)
	ipNum := ipToInt(firstIP)
	ownIP := ipToInt(alloc.network.IP)
	size := networkSize(alloc.network.Mask)

	pos := int32(1)
	max := size - 2 // -1 for the broadcast address, -1 for the gateway address
	for {
		var (
			newNum int32
			inUse  bool
		)

		// Find first unused IP, give up after one whole round
		for attempt := int32(0); attempt < max; attempt++ {
			newNum = ipNum + pos

			pos = pos%max + 1

			// The network's IP is never okay to use
			if newNum == ownIP {
				continue
			}

			if _, inUse = alloc.inUse[newNum]; !inUse {
				// We found an unused IP
				break
			}
		}

		ip := allocatedIP{ip: intToIP(newNum)}
		if inUse {
			ip.err = errors.New("No unallocated IP available")
		}

		select {
		case quit := <-alloc.quit:
			if quit {
				return
			}
		case alloc.queueAlloc <- ip:
			alloc.inUse[newNum] = struct{}{}
		case released := <-alloc.queueReleased:
			r := ipToInt(released)
			delete(alloc.inUse, r)

			if inUse {
				// If we couldn't allocate a new IP, the released one
				// will be the only free one now, so instantly use it
				// next time
				pos = r - ipNum
			} else {
				// Use same IP as last time
				if pos == 1 {
					pos = max
				} else {
					pos--
				}
			}
		}
	}
}

func (alloc *IPAllocator) Acquire() (net.IP, error) {
	ip := <-alloc.queueAlloc
	return ip.ip, ip.err
}

func (alloc *IPAllocator) Release(ip net.IP) {
	alloc.queueReleased <- ip
}

func (alloc *IPAllocator) Close() error {
	alloc.quit <- true
	close(alloc.quit)
	close(alloc.queueAlloc)
	close(alloc.queueReleased)
	return nil
}

func newIPAllocator(network *net.IPNet) *IPAllocator {
	alloc := &IPAllocator{
		network:       network,
		queueAlloc:    make(chan allocatedIP),
		queueReleased: make(chan net.IP),
		inUse:         make(map[int32]struct{}),
		quit:          make(chan bool),
	}

	go alloc.run()

	return alloc
}

// Network interface represents the networking stack of a container
type NetworkInterface struct {
	IPNet   net.IPNet
	Gateway net.IP

	manager  *NetworkManager
	extPorts []*Nat
	disabled bool
}

// Allocate an external port and map it to the interface
func (iface *NetworkInterface) AllocatePort(port Port, binding PortBinding) (*Nat, error) {

	if iface.disabled {
		return nil, fmt.Errorf("Trying to allocate port for interface %v, which is disabled", iface) // FIXME
	}

	ip := iface.manager.portMapper.defaultIp

	if binding.HostIp != "" {
		ip = net.ParseIP(binding.HostIp)
	} else {
		binding.HostIp = ip.String()
	}

	nat := &Nat{
		Port:    port,
		Binding: binding,
	}

	containerPort, err := parsePort(port.Port())
	if err != nil {
		return nil, err
	}

	hostPort, _ := parsePort(nat.Binding.HostPort)

	if nat.Port.Proto() == "tcp" {
		extPort, err := iface.manager.tcpPortAllocator.Acquire(hostPort)
		if err != nil {
			return nil, err
		}

		backend := &net.TCPAddr{IP: iface.IPNet.IP, Port: containerPort}
		if err := iface.manager.portMapper.Map(ip, extPort, backend); err != nil {
			iface.manager.tcpPortAllocator.Release(extPort)
			return nil, err
		}
		nat.Binding.HostPort = strconv.Itoa(extPort)
	} else {
		extPort, err := iface.manager.udpPortAllocator.Acquire(hostPort)
		if err != nil {
			return nil, err
		}
		backend := &net.UDPAddr{IP: iface.IPNet.IP, Port: containerPort}
		if err := iface.manager.portMapper.Map(ip, extPort, backend); err != nil {
			iface.manager.udpPortAllocator.Release(extPort)
			return nil, err
		}
		nat.Binding.HostPort = strconv.Itoa(extPort)
	}
	iface.extPorts = append(iface.extPorts, nat)

	return nat, nil
}

type Nat struct {
	Port    Port
	Binding PortBinding
}

func (n *Nat) String() string {
	return fmt.Sprintf("%s:%d:%d/%s", n.Binding.HostIp, n.Binding.HostPort, n.Port.Port(), n.Port.Proto())
}

// Release: Network cleanup - release all resources
func (iface *NetworkInterface) Release() {
	if iface.disabled {
		return
	}

	for _, nat := range iface.extPorts {
		hostPort, err := parsePort(nat.Binding.HostPort)
		if err != nil {
			log.Printf("Unable to get host port: %s", err)
			continue
		}
		ip := net.ParseIP(nat.Binding.HostIp)
		utils.Debugf("Unmaping %s/%s", nat.Port.Proto, nat.Binding.HostPort)
		if err := iface.manager.portMapper.Unmap(ip, hostPort, nat.Port.Proto()); err != nil {
			log.Printf("Unable to unmap port %s: %s", nat, err)
		}
		if nat.Port.Proto() == "tcp" {
			if err := iface.manager.tcpPortAllocator.Release(hostPort); err != nil {
				log.Printf("Unable to release port %s", nat)
			}
		} else if err := iface.manager.udpPortAllocator.Release(hostPort); err != nil {
			log.Printf("Unable to release port %s: %s", nat, err)
		}
	}

	iface.manager.ipAllocator.Release(iface.IPNet.IP)
}

// Network Manager manages a set of network interfaces
// Only *one* manager per host machine should be used
type NetworkManager struct {
	bridgeIface   string
	bridgeNetwork *net.IPNet

	ipAllocator      *IPAllocator
	tcpPortAllocator *PortAllocator
	udpPortAllocator *PortAllocator
	portMapper       *PortMapper

	disabled bool
}

// Allocate a network interface
func (manager *NetworkManager) Allocate() (*NetworkInterface, error) {

	if manager.disabled {
		return &NetworkInterface{disabled: true}, nil
	}

	var ip net.IP
	var err error

	ip, err = manager.ipAllocator.Acquire()
	if err != nil {
		return nil, err
	}
	// avoid duplicate IP
	ipNum := ipToInt(ip)
	firstIP := manager.ipAllocator.network.IP.To4().Mask(manager.ipAllocator.network.Mask)
	firstIPNum := ipToInt(firstIP) + 1

	if firstIPNum == ipNum {
		ip, err = manager.ipAllocator.Acquire()
		if err != nil {
			return nil, err
		}
	}

	iface := &NetworkInterface{
		IPNet:   net.IPNet{IP: ip, Mask: manager.bridgeNetwork.Mask},
		Gateway: manager.bridgeNetwork.IP,
		manager: manager,
	}
	return iface, nil
}

func (manager *NetworkManager) Close() error {
	err1 := manager.tcpPortAllocator.Close()
	err2 := manager.udpPortAllocator.Close()
	err3 := manager.ipAllocator.Close()
	if err1 != nil {
		return err1
	}
	if err2 != nil {
		return err2
	}
	return err3
}

func newNetworkManager(config *DaemonConfig) (*NetworkManager, error) {
	if config.BridgeIface == DisableNetworkBridge {
		manager := &NetworkManager{
			disabled: true,
		}
		return manager, nil
	}

	addr, err := getIfaceAddr(config.BridgeIface)
	if err != nil {
		// If the iface is not found, try to create it
		if err := CreateBridgeIface(config); err != nil {
			return nil, err
		}
		addr, err = getIfaceAddr(config.BridgeIface)
		if err != nil {
			return nil, err
		}
	}
	network := addr.(*net.IPNet)

	// Configure iptables for link support
	if config.EnableIptables {
		args := []string{"FORWARD", "-i", config.BridgeIface, "-o", config.BridgeIface, "-j", "DROP"}

		if !config.InterContainerCommunication {
			if !iptables.Exists(args...) {
				utils.Debugf("Disable inter-container communication")
				if output, err := iptables.Raw(append([]string{"-A"}, args...)...); err != nil {
					return nil, fmt.Errorf("Unable to prevent intercontainer communication: %s", err)
				} else if len(output) != 0 {
					return nil, fmt.Errorf("Error enabling iptables: %s", output)
				}
			}
		} else {
			utils.Debugf("Enable inter-container communication")
			iptables.Raw(append([]string{"-D"}, args...)...)
		}
	}

	ipAllocator := newIPAllocator(network)

	tcpPortAllocator, err := newPortAllocator()
	if err != nil {
		return nil, err
	}
	udpPortAllocator, err := newPortAllocator()
	if err != nil {
		return nil, err
	}

	portMapper, err := newPortMapper(config)
	if err != nil {
		return nil, err
	}

	manager := &NetworkManager{
		bridgeIface:      config.BridgeIface,
		bridgeNetwork:    network,
		ipAllocator:      ipAllocator,
		tcpPortAllocator: tcpPortAllocator,
		udpPortAllocator: udpPortAllocator,
		portMapper:       portMapper,
	}

	return manager, nil
}
