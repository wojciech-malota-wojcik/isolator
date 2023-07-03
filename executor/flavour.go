package executor

import (
	"context"
	"os"

	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
	"github.com/outofforest/run"
	"github.com/pkg/errors"
)

// DefaultArg is the default CLI arg for starting the executor server.
const DefaultArg = "isolator"

// Config is the config of executor server.
type Config struct {
	// Roouter provide handlers for commands.
	Router Router

	// ExecutorArg is the CLI arg on calling binary which starts the executor server.
	// See `executor.Catch`.
	ExecutorArg string
}

// NewFlavour returns a flavour catching a request to start the executor server. If command is not related,
// then the standard application function is called.
func NewFlavour(config Config) run.FlavourFunc {
	return func(ctx context.Context, appFunc parallel.Task) error {
		if config.ExecutorArg == "" {
			config.ExecutorArg = DefaultArg
		}

		if len(os.Args) < 2 || os.Args[1] != config.ExecutorArg {
			return appFunc(ctx)
		}
		if len(os.Args) != 3 {
			return errors.New("exactly three arguments are required")
		}

		return runServer(logger.WithLogger(ctx, logger.Get(ctx).Named("executor")), config, os.Args[2])
	}
}
