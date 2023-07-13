package main

import (
	"context"
	"net"
	"os"

	"github.com/outofforest/logger"
	"github.com/outofforest/run"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/outofforest/isolator"
	"github.com/outofforest/isolator/executor"
	"github.com/outofforest/isolator/network"
	"github.com/outofforest/isolator/wire"
)

func main() {
	run.New().WithFlavour(executor.NewFlavour(executor.Config{
		// Define commands recognized by the executor server.
		Router: executor.NewRouter().
			RegisterHandler(wire.Execute{}, executor.ExecuteHandler),
	})).Run("example", func(ctx context.Context) (retErr error) {
		log := logger.Get(ctx)
		rootDir := "/tmp/example-execute"
		mountedDir := "/tmp/mount"

		if err := os.MkdirAll(mountedDir, 0o700); err != nil {
			return errors.WithStack(err)
		}

		ip, clean, err := network.Random(30)
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

		config := isolator.Config{
			// Directory where container is created, filesystem of container should exist inside "root" directory there
			Dir: rootDir,
			Types: []interface{}{
				wire.Result{},
				wire.Log{},
			},
			Executor: wire.Config{
				ConfigureSystem: true,
				IP:              ip,
				Hostname:        "isolated-machine",
				Hosts: map[string]net.IP{
					"brother": net.IPv4(1, 1, 1, 1),
				},
				Mounts: []wire.Mount{
					// Let's make host's /tmp/mount available inside container under /test
					{
						Host:      mountedDir,
						Namespace: "/test",
						Writable:  true,
					},
					// To be able to execute system commands
					{
						Host:      "/bin",
						Namespace: "/bin",
					},
					{
						Host:      "/usr/bin",
						Namespace: "/usr/bin",
					},
					{
						Host:      "/usr/sbin",
						Namespace: "/usr/sbin",
					},
					{
						Host:      "/lib",
						Namespace: "/lib",
					},
					{
						Host:      "/lib64",
						Namespace: "/lib64",
					},
					{
						Host:      "/etc/pki/tls/certs/ca-bundle.crt",
						Namespace: "/etc/pki/tls/certs/ca-bundle.crt",
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
			case outgoing <- wire.Execute{Command: `hostname && ip a && ip r && curl https://google.com`}:
			}

			// Communication channel loop
			for content := range incoming {
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

			return errors.WithStack(ctx.Err())
		})
	})
}
