package executor

import (
	"context"
	"os"

	"github.com/outofforest/logger"
	"github.com/outofforest/run"

	"github.com/outofforest/isolator/initprocess"
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

// Catch catches a request to start the executor server. If command is not related, then the function is executed
// to start a standard application.
func Catch(config Config, appFunc func()) {
	if config.ExecutorArg == "" {
		config.ExecutorArg = DefaultArg
	}

	if len(os.Args) < 2 || os.Args[1] != config.ExecutorArg {
		appFunc()
		return
	}

	run.Run("executor", nil, func(ctx context.Context) error {
		if err := logger.Flags(logger.DefaultConfig, "executor").Parse(os.Args[1:]); err != nil {
			return err
		}
		return initprocess.Run(ctx, func(ctx context.Context) error {
			return runServer(ctx, config)
		})
	})
}
