default:
	sudo docker build -t docker-build .
	sudo docker run -lxc-conf=lxc.aa_profile=unconfined -privileged -v `pwd`:/go/src/github.com/dotcloud/docker docker-build hack/make.sh binary

test:
	sudo /etc/init.d/apparmor stop
	sudo /etc/init.d/apparmor teardown
	sudo docker run -lxc-conf=lxc.aa_profile=unconfined -privileged -v `pwd`:/go/src/github.com/dotcloud/docker docker-build hack/make.sh test

shell:
	sudo docker run -privileged -i -t docker-build /bin/bash
