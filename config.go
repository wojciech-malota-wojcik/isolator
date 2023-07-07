package isolator

import "github.com/outofforest/isolator/wire"

// Config stores configuration of isolator
type Config struct {
	// Types defines the list of allowed types transferred between isolator and executor server.
	Types []interface{}

	// ExecutorArg is the CLI arg on calling binary which starts the executor server.
	// See `executor.Catch`.
	ExecutorArg string

	// Directory where root filesystem exists
	Dir string

	// Executor stores configuration passed to executor
	Executor wire.Config
}
