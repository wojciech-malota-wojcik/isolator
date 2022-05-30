package main

import (
	"context"
	"os"

	"github.com/outofforest/logger"
	"github.com/outofforest/run"

	"github.com/outofforest/isolator/executor"
)

func main() {
	run.Tool("executor", nil, func(ctx context.Context) error {
		if err := logger.Flags(logger.ToolDefaultConfig, "executor").Parse(os.Args[1:]); err != nil {
			return err
		}
		return executor.Run(ctx)
	})
}
