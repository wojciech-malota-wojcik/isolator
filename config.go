package isolator

import (
	"net"

	"github.com/outofforest/isolator/wire"
)

// Config stores configuration of isolator.
type Config struct {
	// Types defines the list of allowed types transferred between isolator and executor server.
	Types []interface{}

	// ExecutorArg is the CLI arg on calling binary which starts the executor server.
	// See `executor.Catch`.
	ExecutorArg string

	// Directory where root filesystem exists.
	Dir string

	// ExposedPorts is the list of ports to expose.
	ExposedPorts []ExposedPort

	// Executor stores configuration passed to executor.
	Executor wire.Config
}

// ExposedPort defines a port to be exposed from the namespace.
type ExposedPort struct {
	Protocol     string
	ExternalIP   net.IP
	ExternalPort uint16
	InternalPort uint16
	Public       bool
}
