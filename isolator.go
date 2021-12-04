package isolator

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/wojciech-malota-wojcik/isolator/client"
	"github.com/wojciech-malota-wojcik/isolator/generated"
)

const capSysAdmin = 21
const executorPath = ".executor"

// Start dumps executor to file, starts it, connects to it and returns client
func Start(ctx context.Context, dir string) (c *client.Client, cleanerFn func() error, retErr error) {
	outPipe := newPipe()
	inPipe := newPipe()

	var terminateExecutor func() error
	cleanerFnTmp := func() error {
		outPipe.Close()
		inPipe.Close()
		if terminateExecutor != nil {
			if err := terminateExecutor(); err != nil {
				return fmt.Errorf("terminating executor failed: %w", err)
			}
		}
		return nil
	}
	defer func() {
		if retErr != nil {
			_ = cleanerFnTmp()
		}
	}()

	var err error
	terminateExecutor, err = startExecutor(dir, outPipe, inPipe)
	if err != nil {
		return nil, nil, err
	}
	return client.New(outPipe, inPipe), cleanerFnTmp, nil
}

func startExecutor(dir string, outPipe io.Writer, inPipe io.Reader) (func() error, error) {
	errCh := make(chan error, 1)

	executorPath := filepath.Join(dir, executorPath)

	if err := saveExecutor(executorPath); err != nil {
		defer close(errCh)
		return nil, fmt.Errorf("saving executor executable failed: %w", err)
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

	cmd.Stdout = outPipe
	cmd.Stdin = inPipe
	cmd.Stderr = os.Stderr

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
	}, nil
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

func newPipe() *pipe {
	return &pipe{
		ch: make(chan []byte),
	}
}

type pipe struct {
	mu     sync.RWMutex
	closed bool
	ch     chan []byte
	buf    []byte
}

func (p *pipe) Read(data []byte) (int, error) {
	if len(p.buf) == 0 {
		var ok bool
		p.buf, ok = <-p.ch
		if !ok {
			return 0, io.EOF
		}
	}
	length := len(p.buf)
	if length > len(data) {
		length = len(data)
	}
	copy(data, p.buf[:length])
	p.buf = p.buf[length:]
	return length, nil
}

func (p *pipe) Write(data []byte) (int, error) {
	p.mu.RLock()
	defer p.mu.Unlock()

	if p.closed {
		return 0, io.ErrClosedPipe
	}

	p.ch <- data
	return len(data), nil
}

func (p *pipe) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	p.closed = true
	close(p.ch)
}
