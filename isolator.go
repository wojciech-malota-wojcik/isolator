package isolator

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pkg/errors"

	"github.com/outofforest/isolator/client"
	"github.com/outofforest/isolator/generated"
)

const capSysAdmin = 21

// Start dumps executor to file, starts it, connects to it and returns client
func Start(config Config) (c *client.Client, cleanerFn func() error, retErr error) {
	config, err := sanitizeConfig(config)
	if err != nil {
		return nil, nil, err
	}

	outPipe := newPipe()
	inPipe := newPipe()

	var terminateExecutor func() error
	cleanerFnTmp := func() error {
		outPipe.Close()
		inPipe.Close()
		if terminateExecutor != nil {
			if err := terminateExecutor(); err != nil {
				return errors.WithStack(fmt.Errorf("terminating executor failed: %w", err))
			}
		}
		return nil
	}
	defer func() {
		if retErr != nil {
			if err := cleanerFnTmp(); err != nil {
				retErr = err
			}
		}
	}()

	terminateExecutor, err = startExecutor(config, outPipe, inPipe)
	if err != nil {
		return nil, nil, err
	}

	c = client.New(outPipe, inPipe)
	if err := c.Send(config.Executor); err != nil {
		return nil, nil, errors.WithStack(fmt.Errorf("sending config to executor failed: %w", err))
	}
	return c, cleanerFnTmp, nil
}

func sanitizeConfig(config Config) (Config, error) {
	for i, m := range config.Executor.Mounts {
		var err error
		config.Executor.Mounts[i].Host, err = filepath.Abs(m.Host)
		if err != nil {
			return Config{}, err
		}
	}
	return config, nil
}

func startExecutor(config Config, outPipe io.WriteCloser, inPipe io.ReadCloser) (func() error, error) {
	errCh := make(chan error, 1)

	executorPath, err := saveExecutor()
	if err != nil {
		defer close(errCh)
		return nil, errors.WithStack(fmt.Errorf("saving executor executable failed: %w", err))
	}

	cmd := exec.Command(executorPath)
	cmd.Dir = config.Dir
	// FIXME (wojciech): Remove HOME once logger is fixed
	cmd.Env = []string{"HOME=/", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		//nolint:nosnakecase // Dependency
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUSER | syscall.CLONE_NEWIPC | syscall.CLONE_NEWUTS,
		// by adding CAP_SYS_ADMIN executor may mount /proc
		AmbientCaps: []uintptr{capSysAdmin},
		UidMappings: []syscall.SysProcIDMap{
			{
				HostID:      0,
				ContainerID: 0,
				Size:        65535,
			},
		},
		GidMappingsEnableSetgroups: true,
		GidMappings: []syscall.SysProcIDMap{
			{
				HostID:      0,
				ContainerID: 0,
				Size:        65535,
			},
		},
	}

	cmd.Stdout = outPipe
	cmd.Stdin = inPipe
	cmd.Stderr = os.Stderr

	started := make(chan struct{})
	go func() {
		defer inPipe.Close()
		defer outPipe.Close()

		err := cmd.Start()
		if err != nil {
			errCh <- errors.WithStack(fmt.Errorf("executor error: %w", err))
			close(errCh)
			return
		}
		close(started)
		// cmd.Process.Wait is used because cmd.Wait exits only after cmd.Stdout is closed by user which creates chicke and egg problem
		_, err = cmd.Process.Wait()
		errCh <- err
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
			return errors.WithStack(fmt.Errorf("executor failed: %w", err))
		case <-started:
			// Executor runs with PID 1 inside namespace. From the perspective of kernel it is an init process.
			// Init process receives only signals it subscribed to. So it may happen that SIGTERM is sent before executor
			// subscribes to it. That's why SIGTERM is sent periodically here.
			for {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					return errors.WithStack(fmt.Errorf("sending sigterm to executor failed: %w", err))
				}
				select {
				case err := <-errCh:
					if err != nil {
						return errors.WithStack(fmt.Errorf("executor failed: %w", err))
					}
					return nil
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
	}, nil
}

func saveExecutor() (string, error) {
	file, err := os.CreateTemp("", "executor-*")
	if err != nil {
		return "", errors.WithStack(err)
	}
	defer file.Close()

	if err := os.Chmod(file.Name(), 0o700); err != nil {
		return "", errors.WithStack(fmt.Errorf("making executor executable failed: %w", err))
	}

	gzr, err := gzip.NewReader(base64.NewDecoder(base64.RawStdEncoding, bytes.NewReader([]byte(generated.Executor))))
	if err != nil {
		return "", errors.WithStack(err)
	}
	defer gzr.Close()

	_, err = io.Copy(file, gzr)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return file.Name(), nil
}

func newPipe() *pipe {
	return &pipe{
		closed: make(chan struct{}),
		ch:     make(chan []byte),
	}
}

type pipe struct {
	closed chan struct{}
	ch     chan []byte
	buf    []byte
}

func (p *pipe) Read(data []byte) (n int, retErr error) {
	copied := 0
	for {
		if len(p.buf) == 0 {
			select {
			case <-p.closed:
				return 0, io.EOF
			case p.buf = <-p.ch:
			default:
				if copied > 0 {
					return copied, nil
				}
				select {
				case <-p.closed:
					return 0, io.EOF
				case p.buf = <-p.ch:
				}
			}
		}

		length := len(p.buf)
		if length > len(data)-copied {
			length = len(data) - copied
		}

		copy(data[copied:], p.buf[:length])
		p.buf = p.buf[length:]
		copied += length

		if copied == len(data) {
			return copied, nil
		}
	}
}

func (p *pipe) Write(data []byte) (int, error) {
	// data have to be copied because received array may be a reusable buffer
	buf := make([]byte, len(data))
	copy(buf, data)
	select {
	case <-p.closed:
		return 0, io.ErrClosedPipe
	case p.ch <- buf:
		return len(buf), nil
	}
}

func (p *pipe) Close() error {
	select {
	case <-p.closed:
	default:
		close(p.closed)
	}
	return nil
}
