package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/ridge/parallel"
	"github.com/wojciech-malota-wojcik/isolator/client"
	"github.com/wojciech-malota-wojcik/isolator/client/wire"
	"github.com/wojciech-malota-wojcik/libexec"
	"github.com/wojciech-malota-wojcik/logger"
	"go.uber.org/zap"
)

type logTransmitter struct {
	client *client.Client
	stream wire.Stream

	buf []byte
}

func (lt *logTransmitter) Write(data []byte) (int, error) {
	length := len(lt.buf) + len(data)
	if length < 100 {
		lt.buf = append(lt.buf, data...)
		return len(data), nil
	}
	buf := make([]byte, length)
	copy(buf, lt.buf)
	copy(buf[len(lt.buf):], data)
	err := lt.client.Send(wire.Log{Stream: lt.stream, Text: string(buf)})
	if err != nil {
		return 0, err
	}
	lt.buf = make([]byte, 0, len(lt.buf))
	return len(data), nil
}

func (lt *logTransmitter) Flush() error {
	if len(lt.buf) == 0 {
		return nil
	}
	if err := lt.client.Send(wire.Log{Stream: lt.stream, Text: string(lt.buf)}); err != nil {
		return err
	}
	lt.buf = make([]byte, 0, len(lt.buf))
	return nil
}

// Run runs isolator server
func Run(ctx context.Context) (retErr error) {
	log := logger.Get(ctx)

	// systemd remounts everything as MS_SHARED, to rpevent mess let's remount everything back to MS_PRIVATE inside namespace
	if err := syscall.Mount("", "/", "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("remounting / as slave failed: %w", err)
	}

	// PivotRoot can't be applied to directory where namespace was created, let's create subdirectory
	if err := os.Mkdir("root", 0o755); err != nil && !os.IsExist(err) {
		return err
	}

	// PivotRoot requires new root to be on different mountpoint, so let's bind it to itself
	if err := syscall.Mount("root", "root", "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("binding new root to itself failed: %w", err)
	}

	// Let's assume that new filesystem is the current working dir even before pivoting to make life easier
	if err := os.Chdir("root"); err != nil {
		return err
	}

	// Looks like PivotRoot drops CAP_SYS_ADMIN for some weird reason so mounting proc has to be done before that
	if err := os.Mkdir("proc", 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	if err := syscall.Mount("none", "proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("mounting proc failed: %w", err)
	}
	defer func() {
		// no matter if we have already pivoted or not, proc is in current working dir and it's possible to unmount it
		if err := syscall.Unmount("proc", 0); err != nil {
			log.Error("Unmounting proc failed", zap.Error(err))
			if retErr == nil {
				retErr = fmt.Errorf("unmounting proc failed: %w", err)
			}
		}
	}()

	// starting unix socket before pivoting so we may create it in upper directory
	l, err := net.Listen("unix", filepath.Join("..", wire.SocketPath))
	if err != nil {
		return fmt.Errorf("failed to start listening: %w", err)
	}
	defer func() {
		if l != nil {
			if err := l.Close(); err != nil {
				log.Error("Closing listening socket failed", zap.Error(err))
			}
		}
	}()

	if err := os.Mkdir(".old", 0o700); err != nil {
		return err
	}
	if err := syscall.PivotRoot(".", ".old"); err != nil {
		return fmt.Errorf("pivoting / failed: %w", err)
	}
	if err := syscall.Mount("", ".old", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("remounting .old as private failed: %w", err)
	}
	if err := syscall.Unmount(".old", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmounting .old failed: %w", err)
	}
	if err := os.Remove(".old"); err != nil {
		return err
	}

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
			if ctx.Err() != nil {
				_ = connTmp.Close()
				mu.Unlock()
				return ctx.Err()
			}
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

				outTransmitter := &logTransmitter{
					stream: wire.StreamOut,
					client: c,
				}
				errTransmitter := &logTransmitter{
					stream: wire.StreamErr,
					client: c,
				}

				cmd := exec.Command("/bin/sh", "-c", execute.Command)
				cmd.Stdout = outTransmitter
				cmd.Stderr = errTransmitter
				var errStr string
				if err := libexec.Exec(ctx, cmd); err != nil {
					errStr = err.Error()
				}
				if err := outTransmitter.Flush(); err != nil {
					log.Error("Flushing stdout log failed", zap.Error(err))
				}
				if err := errTransmitter.Flush(); err != nil {
					log.Error("Flushing stderr log failed", zap.Error(err))
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
			if l != nil {
				_ = l.Close()
				l = nil
			}
			return nil
		})
		return nil
	})
}
