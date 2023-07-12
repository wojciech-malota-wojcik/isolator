package main

import (
	"context"
	"os"

	"github.com/outofforest/logger"
	"github.com/outofforest/run"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/outofforest/isolator/executor"
	"github.com/outofforest/isolator/network"
	"github.com/outofforest/isolator/scenarios"
	"github.com/outofforest/isolator/wire"
)

func main() {
	run.New().WithFlavour(executor.NewFlavour(executor.Config{
		// Define commands recognized by the executor server.
		Router: executor.NewRouter().
			RegisterHandler(wire.RunEmbeddedFunction{}, executor.NewRunEmbeddedFunctionHandler(
				map[string]executor.EmbeddedFunc{
					"test1": func(ctx context.Context, args []string) error {
						logger.Get(ctx).Info("Test1", zap.String("arg", args[0]))
						<-ctx.Done()
						return errors.WithStack(ctx.Err())
					},
					"test2": func(ctx context.Context, args []string) error {
						logger.Get(ctx).Info("Test2",
							zap.String("arg0", args[0]),
							zap.String("arg1", args[1]))
						<-ctx.Done()
						return errors.WithStack(ctx.Err())
					},
				},
			)),
	})).Run("example", func(ctx context.Context) (retErr error) {
		log := logger.Get(ctx)
		appDir := "/tmp/example-embedded"

		if err := os.RemoveAll(appDir); err != nil && !os.IsNotExist(err) {
			return errors.WithStack(err)
		}

		appNetwork, clean, err := network.Random(24)
		if err != nil {
			return err
		}
		defer func() {
			if err := clean(); err != nil {
				if retErr == nil {
					retErr = err
				}
				log.Error("Cleaning network failed", zap.Error(err))
			}
		}()

		return scenarios.RunApps(ctx, scenarios.RunAppsConfig{
			AppsDir: appDir,
		}, scenarios.Embedded{
			IP:   network.Addr(appNetwork, 2),
			Name: "test1",
			Args: []string{"value"},
		}, scenarios.Embedded{
			IP:   network.Addr(appNetwork, 3),
			Name: "test2",
			Args: []string{"value0", "value1"},
		})
	})
}
