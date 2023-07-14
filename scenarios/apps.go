package scenarios

import (
	"context"
	"net"

	"github.com/outofforest/parallel"
	"github.com/pkg/errors"

	"github.com/outofforest/isolator/lib/task"
)

// RunAppsConfig is the config for running applications.
type RunAppsConfig struct {
	CacheDir string
	AppsDir  string
}

// Application represents an app to run in isolation.
type Application interface {
	GetName() string
	GetIP() net.IP
	GetTaskFunc(config RunAppsConfig, appHosts map[string]net.IP, spawn parallel.SpawnFn) task.Func
}

// RunApps runs applications.
func RunApps(ctx context.Context, config RunAppsConfig, apps ...Application) error {
	containerHosts := map[string]net.IP{}
	for _, app := range apps {
		if app.GetName() != "" && app.GetIP() != nil {
			containerHosts[app.GetName()] = app.GetIP()
		}
	}

	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		spawn("start", parallel.Continue, func(ctx context.Context) error {
			return task.Run(ctx, nil, func(ctx context.Context, taskCh chan<- task.Task, doneCh <-chan task.Task) error {
				for _, app := range apps {
					select {
					case <-ctx.Done():
						return errors.WithStack(ctx.Err())
					case taskCh <- task.Task{
						ID: "app:run:" + app.GetName(),
						Do: app.GetTaskFunc(config, containerHosts, spawn),
					}:
					}
				}

				return nil
			})
		})
		return nil
	})
}
