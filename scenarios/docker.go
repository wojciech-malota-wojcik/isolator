package scenarios

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/outofforest/isolator"
	"github.com/outofforest/isolator/lib/task"
	"github.com/outofforest/isolator/wire"
)

// Container defines container.
type Container struct {
	// Name is the name of the container.
	Name string

	// Image is the name of the image.
	Image string

	// Tag is the tag of the image.
	Tag string

	// EnvVars sets environment variables inside container.
	EnvVars map[string]string

	// User is the username or UID to use.
	User string

	// Mounts are the mounts to configure inside container.
	Mounts []Mount

	// WorkingDir specifies a path to working directory.
	WorkingDir string

	// Entrypoint sets entrypoint for container.
	Entrypoint []string

	// Args sets args for container.
	Args []string

	// IP is the IP address of the container.
	IP net.IP
}

// Mount defines the mount to be configured inside container.
type Mount struct {
	Host      string
	Container string
	Writable  bool
}

// RunContainerConfig is the config for running containers.
type RunContainerConfig struct {
	CacheDir     string
	ContainerDir string
}

// RunContainers run containers.
func RunContainers(ctx context.Context, config RunContainerConfig, containers ...Container) error {
	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		parentSpawn := spawn

		spawn("start", parallel.Continue, func(ctx context.Context) error {
			return task.Run(ctx, nil, func(ctx context.Context, taskCh chan<- task.Task, doneCh <-chan task.Task) error {
				for _, c := range containers {
					c := c

					select {
					case <-ctx.Done():
						return errors.WithStack(ctx.Err())
					case taskCh <- task.Task{
						ID: "container:run:" + c.Name,
						Do: func(ctx context.Context) error {
							ctx = logger.With(ctx, zap.String("container", c.Name))

							if err := os.MkdirAll(config.ContainerDir, 0o700); err != nil {
								return errors.WithStack(err)
							}
							containerDir := filepath.Join(config.ContainerDir, c.Name)
							// 0o755 mode is essential here. Without this, container running as non-root user will
							// fail with "permission denied"
							if err := os.Mkdir(containerDir, 0o755); err != nil {
								return errors.WithStack(err)
							}

							if err := os.MkdirAll(config.CacheDir, 0o700); err != nil {
								return errors.WithStack(err)
							}

							err := isolator.Run(ctx, isolator.Config{
								Dir: containerDir,
								Types: []interface{}{
									wire.Result{},
								},
								Executor: wire.Config{
									Mounts: []wire.Mount{
										{
											Host:      config.CacheDir,
											Container: "/.cache",
											Writable:  true,
										},
									},
								},
							}, func(ctx context.Context, incoming <-chan interface{}, outgoing chan<- interface{}) error {
								log := logger.Get(ctx)
								log.Info("Inflating container's filesystem")

								select {
								case <-ctx.Done():
									return errors.WithStack(ctx.Err())
								case outgoing <- wire.InflateDockerImage{
									CacheDir: "/.cache",
									Image:    c.Image,
									Tag:      c.Tag,
								}:
								}

								for {
									var content interface{}
									var ok bool

									select {
									case <-ctx.Done():
										return errors.WithStack(ctx.Err())
									case content, ok = <-incoming:
									}

									if !ok {
										return errors.WithStack(ctx.Err())
									}

									switch m := content.(type) {
									// wire.Result means command finished
									case wire.Result:
										if m.Error != "" {
											return errors.Errorf("inflating image failed: %s", m.Error)
										}
										return nil
									default:
										panic("unexpected message received")
									}
								}
							})
							if err != nil {
								return err
							}

							parentSpawn(fmt.Sprintf("container-%s", c.Name), parallel.Fail, func(ctx context.Context) error {
								config := isolator.Config{
									Dir: containerDir,
									Types: []interface{}{
										wire.Log{},
										wire.Result{},
									},
									Executor: wire.Config{
										ConfigureSystem: true,
										Mounts: []wire.Mount{
											{
												Host:      config.CacheDir,
												Container: "/.cache",
												Writable:  true,
											},
										},
									},
								}

								for _, m := range c.Mounts {
									config.Executor.Mounts = append(config.Executor.Mounts, wire.Mount{
										Host:      m.Host,
										Container: m.Container,
										Writable:  m.Writable,
									})
								}

								return isolator.Run(ctx, config, func(ctx context.Context, incoming <-chan interface{}, outgoing chan<- interface{}) error {
									log := logger.Get(ctx)
									log.Info("Starting container")

									select {
									case <-ctx.Done():
										return errors.WithStack(ctx.Err())
									case outgoing <- wire.RunDockerContainer{
										CacheDir:   "/.cache",
										Name:       c.Name,
										Image:      c.Image,
										Tag:        c.Tag,
										EnvVars:    c.EnvVars,
										User:       c.User,
										WorkingDir: c.WorkingDir,
										Entrypoint: c.Entrypoint,
										Args:       c.Args,
									}:
									}

									for {
										var content interface{}
										var ok bool

										select {
										case <-ctx.Done():
											return errors.WithStack(ctx.Err())
										case content, ok = <-incoming:
										}

										if !ok {
											return errors.WithStack(ctx.Err())
										}

										switch m := content.(type) {
										// wire.Log contains message printed by executed command to stdout or stderr
										case wire.Log:
											stream, err := wire.ToStream(m.Stream)
											if err != nil {
												panic(err)
											}
											if _, err := stream.WriteString(m.Text); err != nil {
												panic(err)
											}
										// wire.Result means command finished
										case wire.Result:
											if m.Error != "" {
												return errors.Errorf("container failed: %s", m.Error)
											}
											return nil
										default:
											panic("unexpected message received")
										}
									}
								})
							})
							return nil
						},
					}:
					}
				}

				return nil
			})
		})
		return nil
	})
}
