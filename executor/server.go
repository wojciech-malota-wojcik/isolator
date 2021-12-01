package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/wojciech-malota-wojcik/isolator/client"

	"github.com/ridge/parallel"
	"github.com/wojciech-malota-wojcik/isolator/executor/wire"
	"github.com/wojciech-malota-wojcik/libexec"
)

// Run runs isolator server
func Run(ctx context.Context) (retErr error) {
	if err := os.MkdirAll("/proc", 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	if err := syscall.Mount("none", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("mounting proc failed: %w", err)
	}
	defer func() {
		if err := syscall.Unmount("/proc", 0); retErr == nil {
			retErr = err
		}
	}()

	l, err := net.Listen("unix", wire.SocketPath)
	if err != nil {
		return fmt.Errorf("failed to start listening: %w", err)
	}
	defer func() {
		_ = l.Close()
		_ = os.Remove(wire.SocketPath)
	}()

	var mu sync.Mutex
	var conn net.Conn
	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		spawn("connection", parallel.Exit, func(ctx context.Context) error {
			var err error
			connTmp, err := l.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("accepting connection failed: %w", err)
			}

			mu.Lock()
			conn = connTmp
			mu.Unlock()

			c := client.New(conn)
			for {
				msg, err := c.Receive()
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if errors.Is(err, io.EOF) {
						return nil
					}
					return fmt.Errorf("receiving message failed: %w", err)
				}
				execute, ok := msg.(wire.Execute)
				if !ok {
					return errors.New("unexpected message received")
				}

				var errStr string
				cmd := exec.Command("/bin/sh", "-c", execute.Command)
				if err := libexec.Exec(ctx, cmd); err != nil {
					errStr = err.Error()
				}
				if err := c.Send(wire.Completed{
					ExitCode: cmd.ProcessState.ExitCode(),
					Error:    errStr,
				}); err != nil {
					return fmt.Errorf("command status reporting failed: %w", err)
				}
			}
		})
		spawn("watchdog", parallel.Exit, func(ctx context.Context) error {
			<-ctx.Done()
			mu.Lock()
			defer mu.Unlock()

			if conn != nil {
				_ = conn.Close()
			}
			return nil
		})
		return nil
	})
}
