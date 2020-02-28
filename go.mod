module github.com/myENA/drprune

go 1.14

replace github.com/cego/docker-registry-pruner v0.0.0-20181210151009-317c18d0333f => ../../nathanejohnson/docker-registry-pruner

require (
	github.com/cego/docker-registry-pruner v0.0.0-20190904085958-83bcaac3e2d2
	github.com/fsouza/go-dockerclient v1.6.3
	github.com/hashicorp/consul/api v1.4.0
	github.com/myENA/consul-decoder v0.2.5
	github.com/rs/zerolog v1.18.0
)
