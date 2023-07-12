package scenarios

import "net"

// Mount defines the mount to be configured inside container.
type Mount struct {
	Host      string
	Namespace string
	Writable  bool
}

// ExposedPort defines a port to be exposed from the container.
type ExposedPort struct {
	Protocol      string
	HostIP        net.IP
	HostPort      uint16
	NamespacePort uint16
	Public        bool
}
