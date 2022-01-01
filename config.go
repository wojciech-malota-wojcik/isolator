package isolator

import "github.com/outofforest/isolator/client/wire"

// Config stores configuration of isolator
type Config struct {
	// Directory where root filesystem exists
	Dir string

	// Executor tores configuration passed to executor
	Executor wire.Config
}
