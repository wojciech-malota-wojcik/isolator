package main

import (
	"context"
	"net"
	"os"
	"path/filepath"

	"github.com/outofforest/logger"
	"github.com/outofforest/run"
	"github.com/pkg/errors"
	"github.com/ridge/must"
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
			RegisterHandler(wire.InflateDockerImage{}, executor.NewInflateDockerImageHandler()).
			RegisterHandler(wire.RunDockerContainer{}, executor.RunDockerContainerHandler),
	})).Run("example", func(ctx context.Context) (retErr error) {
		log := logger.Get(ctx)
		appDir := "/tmp/example-docker"
		cacheDir := filepath.Join(must.String(os.UserCacheDir()), "docker-cache")

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
			CacheDir: cacheDir,
			AppsDir:  appDir,
		}, scenarios.Container{
			IP:    network.Addr(appNetwork, 2),
			Name:  "my-container",
			Image: "grafana/grafana",
			Tag:   "sha256:1caf984a3f2e07ea4f5ffd25c16fad0ed0ddac043467e8b9ddaf4cbbc6299ec4",
			ExposedPorts: []scenarios.ExposedPort{
				{
					Protocol:      "tcp",
					HostIP:        net.IPv4zero,
					HostPort:      80,
					NamespacePort: 3000,
					Public:        true,
				},
			},
		})
	})
}
