package main

import (
	"context"
	"os"

	"github.com/outofforest/run"
	"github.com/pkg/errors"

	"github.com/outofforest/isolator"
	"github.com/outofforest/isolator/executor"
	"github.com/outofforest/isolator/wire"
)

func main() {
	run.New().WithFlavour(executor.NewFlavour(executor.Config{
		// Define commands recognized by the executor server.
		Router: executor.NewRouter().
			RegisterHandler(wire.Execute{}, executor.ExecuteHandler),
	})).Run("example", func(ctx context.Context) error {
		rootDir := "/tmp/example-execute"
		mountedDir := "/tmp/mount"

		if err := os.MkdirAll(mountedDir, 0o700); err != nil {
			return errors.WithStack(err)
		}

		config := isolator.Config{
			// Directory where container is created, filesystem of container should exist inside "root" directory there
			Dir: rootDir,
			Types: []interface{}{
				wire.Result{},
				wire.Log{},
			},
			Executor: wire.Config{
				ConfigureSystem: true,
				Mounts: []wire.Mount{
					// Let's make host's /tmp/mount available inside container under /test
					{
						Host:      mountedDir,
						Container: "/test",
						Writable:  true,
					},
					// To be able to execute /bin/sh
					{
						Host:      "/bin",
						Container: "/bin",
					},
					{
						Host:      "/lib",
						Container: "/lib",
					},
					{
						Host:      "/lib64",
						Container: "/lib64",
					},
				},
			},
		}

		// Starting isolator. If passed ctx is canceled, isolator exits.
		// Isolator creates `root` directory under the one passed to isolator. The `root` directory is mounted as `/`.
		// inside container.
		// It is assumed that `root` contains `bin/sh` shell and all the required libraries. Without them it will fail.

		return isolator.Run(ctx, config, func(ctx context.Context, incoming <-chan interface{}, outgoing chan<- interface{}) error {
			// Request to execute command in isolation
			select {
			case <-ctx.Done():
				return errors.WithStack(ctx.Err())
			case outgoing <- wire.Execute{Command: `echo "Hello world!"`}:
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
						panic(errors.Errorf("command failed: %s", m.Error))
					}
					return nil
				default:
					panic("unexpected message received")
				}
			}
		})
	})
}
