package isolator

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wojciech-malota-wojcik/isolator/client"
	"github.com/wojciech-malota-wojcik/isolator/client/wire"
	"github.com/wojciech-malota-wojcik/isolator/generated"
	"github.com/wojciech-malota-wojcik/logger"
	"go.uber.org/zap"
)

const capSysAdmin = 21
const executorPath = ".executor"

// Start dumps executor to file, starts it, connects to it and returns client
func Start(ctx context.Context, dir string) (c *client.Client, cleanerFn func() error, retErr error) {
	log := logger.Get(ctx)

	var terminateExecutor func() error
	cleanerFnTmp := func() error {
		failed := false
		if terminateExecutor != nil {
			if err := terminateExecutor(); err != nil {
				log.Error("Terminating executor failed", zap.Error(err))
				failed = true
			}
		}
		if failed {
			return errors.New("cleaning executor failed")
		}
		return nil
	}
	defer func() {
		if retErr != nil {
			_ = cleanerFnTmp()
		}
	}()

	var errCh <-chan error
	terminateExecutor, errCh = startExecutor(dir, log)

	var conn net.Conn
	for i := 0; i < 100; i++ {
		select {
		case err := <-errCh:
			return nil, nil, fmt.Errorf("executor exited before connection was made: %w", err)
		default:
		}

		conn, retErr = net.Dial("unix", filepath.Join(dir, wire.SocketPath))
		if retErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if retErr != nil {
		return nil, nil, fmt.Errorf("connecting to executor failed: %w", retErr)
	}
	return client.New(conn), cleanerFnTmp, nil
}

func startExecutor(dir string, log *zap.Logger) (func() error, <-chan error) {
	errCh := make(chan error, 1)

	executorPath := filepath.Join(dir, executorPath)

	if err := saveExecutor(executorPath); err != nil {
		errCh <- fmt.Errorf("saving executor executable failed: %w", err)
		close(errCh)
		return nil, errCh
	}

	cmd := exec.Command(executorPath)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUSER | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS,
		// by adding CAP_SYS_ADMIN executor may mount /proc
		// by adding CAP_MKNOD executor may populate /dev
		AmbientCaps: []uintptr{capSysAdmin},
		UidMappings: []syscall.SysProcIDMap{
			{
				HostID:      os.Getuid(),
				ContainerID: 0,
				Size:        1,
			},
		},
		GidMappings: []syscall.SysProcIDMap{
			{
				HostID:      os.Getgid(),
				ContainerID: 0,
				Size:        1,
			},
		},
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = bytes.NewReader(nil)

	started := make(chan struct{})
	go func() {
		err := cmd.Start()
		if err != nil {
			errCh <- fmt.Errorf("executor error: %w", err)
			close(errCh)
			return
		}
		close(started)
		errCh <- cmd.Wait()
		close(errCh)
	}()

	return func() (retErr error) {
		defer func() {
			if err := os.Remove(executorPath); err != nil && !os.IsNotExist(err) {
				log.Error("Removing executor file failed", zap.Error(err))
				if retErr == nil {
					retErr = err
				}
			}
			if err := os.Remove(filepath.Join(dir, wire.SocketPath)); err != nil && !os.IsNotExist(err) {
				log.Error("Removing unix socket file failed", zap.Error(err))
				if retErr == nil {
					retErr = err
				}
			}
		}()

		select {
		case err := <-errCh:
			if err == nil {
				return nil
			}
			return fmt.Errorf("executor failed: %w", err)
		case <-started:
			// Executor runs with PID 1 inside namespace. From the perspective of kernel it is an init process.
			// Init process receives only signals it subscribed to. So it may happen that SIGTERM is sent before executor
			// subscribes to it. That's why SIGTERM is sent periodically here.
			for {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					return fmt.Errorf("sending sigterm to executor failed: %w", err)
				}
				select {
				case err := <-errCh:
					if err != nil {
						return fmt.Errorf("executor failed: %w", err)
					}
					return nil
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
	}, errCh
}

func saveExecutor(path string) error {
	gzr, err := gzip.NewReader(base64.NewDecoder(base64.RawStdEncoding, bytes.NewReader([]byte(generated.Executor))))
	if err != nil {
		return err
	}
	defer gzr.Close()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, gzr)
	return err
}
