module github.com/myENA/drprune

go 1.14

require (
	github.com/Microsoft/hcsshim v0.8.7 // indirect
	github.com/armon/go-metrics v0.3.2 // indirect
	github.com/cego/docker-registry-pruner v0.0.0-00010101000000-000000000000
	github.com/containerd/containerd v1.3.3 // indirect
	github.com/containerd/continuity v0.0.0-20200107194136-26c1120b8d41 // indirect
	github.com/fsouza/go-dockerclient v1.6.3
	github.com/gogo/protobuf v1.3.1 // indirect
	github.com/golang/protobuf v1.3.4 // indirect
	github.com/hashicorp/consul/api v1.4.0
	github.com/hashicorp/go-immutable-radix v1.1.0 // indirect
	github.com/hashicorp/go-msgpack v0.5.5 // indirect
	github.com/hashicorp/go-sockaddr v1.0.2 // indirect
	github.com/hashicorp/golang-lru v0.5.4 // indirect
	github.com/hashicorp/memberlist v0.1.6 // indirect
	github.com/hashicorp/serf v0.8.5 // indirect
	github.com/konsorten/go-windows-terminal-sequences v1.0.2 // indirect
	github.com/mattn/go-colorable v0.1.6 // indirect
	github.com/miekg/dns v1.1.26 // indirect
	github.com/morikuni/aec v1.0.0 // indirect
	github.com/myENA/consul-decoder v0.2.5
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rs/zerolog v1.18.0
	golang.org/x/crypto v0.0.0-20200221231518-2aa609cf4a9d // indirect
	google.golang.org/genproto v0.0.0-20200228133532-8c2c7df3a383 // indirect
	google.golang.org/grpc v1.27.1 // indirect
)

replace github.com/cego/docker-registry-pruner => github.com/nathanejohnson/docker-registry-pruner v0.0.0-20200304033253-76fc240f6a1c
