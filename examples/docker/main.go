package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/outofforest/parallel"
	"github.com/outofforest/run"
	"github.com/pkg/errors"
	"github.com/ridge/must"

	"github.com/outofforest/isolator"
	"github.com/outofforest/isolator/executor"
	"github.com/outofforest/isolator/wire"
)

func main() {
	run.New().WithFlavour(executor.NewFlavour(executor.Config{
		// Define commands recognized by the executor server.
		Router: executor.NewRouter().
			RegisterHandler(wire.InflateDockerImage{}, executor.NewInflateDockerImageHandler()),
	})).Run("example", func(ctx context.Context) error {
		incoming := make(chan interface{})
		outgoing := make(chan interface{})

		rootDir := "/tmp/example-docker"
		cacheDir := filepath.Join(must.String(os.UserCacheDir()), "docker-cache")

		if err := os.MkdirAll(rootDir, 0o700); err != nil {
			return errors.WithStack(err)
		}
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			return errors.WithStack(err)
		}
		config := isolator.Config{
			// Directory where container is created, filesystem of container should exist inside "root" directory there
			Dir: rootDir,
			Types: []interface{}{
				wire.Result{},
			},
			Executor: wire.Config{
				NoStandardMounts: true,
				Mounts: []wire.Mount{
					{
						Host:      cacheDir,
						Container: "/.cache",
						Writable:  true,
					},
				},
			},
			Incoming: incoming,
			Outgoing: outgoing,
		}

		return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
			spawn("isolator", parallel.Fail, func(ctx context.Context) error {
				// Starting isolator. If passed ctx is canceled, isolator exits.
				// Isolator creates `root` directory under one passed to isolator. The `root` directory is mounted as `/`.
				// inside container.
				// It is assumed that `root` contains `bin/sh` shell and all the required libraries. Without them it will fail.

				return isolator.Run(ctx, config)
			})
			spawn("client", parallel.Exit, func(ctx context.Context) error {
				// Request to execute command in isolation
				select {
				case <-ctx.Done():
					return errors.WithStack(ctx.Err())
				case outgoing <- wire.InflateDockerImage{
					CacheDir: "/.cache",
					Image:    "grafana/grafana",
					Tag:      "latest",
				}:
				}

				// Communication channel loop
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
						stream, err := toStream(m.Stream)
						if err != nil {
							panic(err)
						}
						if _, err := stream.WriteString(m.Text); err != nil {
							panic(err)
						}
					// wire.Result means command finished
					case wire.Result:
						if m.Error != "" {
							panic(errors.Errorf("command failed: %s", m.Error))
						}
						return nil
					default:
						panic("unexpected message received")
					}
				}
			})

			return nil
		})
	})
}

func toStream(stream wire.Stream) (*os.File, error) {
	var f *os.File
	switch stream {
	case wire.StreamOut:
		f = os.Stdout
	case wire.StreamErr:
		f = os.Stderr
	default:
		return nil, errors.Errorf("unknown stream: %d", stream)
	}
	return f, nil
}
