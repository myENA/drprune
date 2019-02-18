module github.com/myENA/drprune

require (
	github.com/cego/docker-registry-pruner v0.0.0-20181210151009-317c18d0333f
	github.com/fsouza/go-dockerclient v1.3.6
	github.com/hashicorp/consul v1.4.2
	github.com/hashicorp/go-cleanhttp v0.5.0 // indirect
	github.com/hashicorp/go-rootcerts v1.0.0 // indirect
	github.com/hashicorp/serf v0.8.2 // indirect
	github.com/mitchellh/go-testing-interface v1.0.0 // indirect
	github.com/mitchellh/mapstructure v1.1.2 // indirect
	github.com/myENA/consul-decoder v0.2.3
	github.com/rs/zerolog v1.11.0
)

replace github.com/cego/docker-registry-pruner v0.0.0-20181210151009-317c18d0333f => ../../nathanejohnson/docker-registry-pruner
