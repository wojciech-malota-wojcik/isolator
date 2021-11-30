package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/ridge/must"
	"github.com/ridge/parallel"
	"github.com/wojciech-malota-wojcik/isolator/executor/wire"
	"github.com/wojciech-malota-wojcik/libexec"
)

// Run runs isolator server
func Run(ctx context.Context) error {
	if err := os.MkdirAll("/proc", 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	if err := syscall.Mount("none", "/proc", "proc", 0, ""); err != nil {
		return err
	}
	defer func() {
		_ = syscall.Unmount("/proc", 0)
	}()

	listener, err := net.Listen("unix", wire.SocketPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(wire.SocketPath)
	}()

	var mu sync.Mutex
	var conn net.Conn
	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		spawn("connection", parallel.Exit, func(ctx context.Context) error {
			var err error
			connTmp, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}

			mu.Lock()
			conn = connTmp
			mu.Unlock()

			decoder := json.NewDecoder(conn)
			for {
				var msg wire.Execute
				if err := decoder.Decode(&msg); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if errors.Is(err, io.EOF) {
						return nil
					}
					return err
				}

				var errStr string
				cmd := exec.Command("/bin/sh", "-c", msg.Command)
				if err := libexec.Exec(ctx, cmd); err != nil {
					errStr = err.Error()
				}
				if _, err := conn.Write(must.Bytes(json.Marshal(wire.Completed{
					ExitCode: cmd.ProcessState.ExitCode(),
					Error:    errStr,
				}))); err != nil {
					return err
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
