package isolator

import "github.com/outofforest/isolator/wire"

// Config stores configuration of isolator
type Config struct {
	// Incoming is the channel where messages received from the executor are delivered to.
	Incoming chan<- interface{}

	// Outgoing is the channel where messages to be delivered to the executor are received from.
	Outgoing <-chan interface{}

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
