package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
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

// Run runs isolator server
func Run(ctx context.Context) (retErr error) {
	log := logger.Get(ctx)

	if err := prepareNewRoot(); err != nil {
		return fmt.Errorf("preparing new root filesystem failed: %w", err)
	}
	if err := mountProc(); err != nil {
		return err
	}
	if err := populateDev(); err != nil {
		return err
	}

	// starting unix socket before pivoting so we may create it in upper directory
	l, err := net.Listen("unix", filepath.Join("..", wire.SocketPath))
	if err != nil {
		return fmt.Errorf("failed to start listening: %w", err)
	}
	defer func() {
		if l != nil {
			if err := l.Close(); err != nil {
				log.Error("Closing listening socket failed", zap.Error(err))
				if retErr == nil {
					retErr = err
				}
			}
		}
	}()

	if err := pivotRoot(); err != nil {
		return fmt.Errorf("pivoting root filesystem failed: %w", err)
	}

	if err := configureDNS(); err != nil {
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

func prepareNewRoot() error {
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
	return os.Chdir("root")
}

func mountProc() error {
	if err := os.Mkdir("proc", 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	if err := syscall.Mount("none", "proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("mounting proc failed: %w", err)
	}
	return nil
}

func populateDev() error {
	devDir := "dev"
	if err := os.MkdirAll(devDir, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	if err := syscall.Mount("none", devDir, "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mounting tmpfs for dev failed: %w", err)
	}
	for _, dev := range []string{"console", "null", "zero", "random", "urandom"} {
		devPath := filepath.Join(devDir, dev)
		f, err := os.OpenFile(devPath, os.O_CREATE|os.O_RDONLY, 0o644)
		if err != nil {
			return fmt.Errorf("creating dev/%s file failed: %w", dev, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("closing dev/%s file failed: %w", dev, err)
		}
		if err := syscall.Mount(filepath.Join("/", devPath), devPath, "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
			return fmt.Errorf("binding dev/%s device failed: %w", dev, err)
		}
	}
	return nil
}

func pivotRoot() error {
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
	return os.Remove(".old")
}

func configureDNS() error {
	if err := os.Mkdir("etc", 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	return ioutil.WriteFile("etc/resolv.conf", []byte("nameserver 8.8.8.8\nnameserver 8.8.4.4\n"), 0o644)
}

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
