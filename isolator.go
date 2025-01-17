package isolator

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/outofforest/isolator/executor"
	"github.com/outofforest/isolator/network"
	"github.com/outofforest/isolator/wire"
	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
)

// ClientFunc defines the client function for isolator.
type ClientFunc func(ctx context.Context, incoming <-chan interface{}, outgoing chan<- interface{}) error

// Run runs executor server and communication channel.
//
//nolint:gocyclo
func Run(ctx context.Context, config Config, clientFunc ClientFunc) error {
	config, err := sanitizeConfig(config)
	if err != nil {
		return err
	}

	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		startCh := make(chan struct{})
		startedCh := make(chan struct{})
		exitedCh := make(chan struct{})
		cmdPIDCh := make(chan int, 1)

		outPipe := newPipe()
		inPipe := newPipe()

		incoming := make(chan interface{})
		outgoing := make(chan interface{})

		cmd := newExecutorServerCommand(config)
		cmd.Stdout = outPipe
		cmd.Stdin = inPipe
		cmd.Stderr = os.Stderr

		spawn("isolator.executor", parallel.Fail, func(ctx context.Context) error {
			defer func() {
				_ = inPipe.Close()
				_ = outPipe.Close()
			}()

			select {
			case <-ctx.Done():
				return errors.WithStack(ctx.Err())
			case <-startCh:
			}

			if err := os.MkdirAll(config.Dir, 0o700); err != nil {
				return errors.WithStack(err)
			}

			if err := cmd.Start(); err != nil {
				return errors.WithStack(err)
			}

			cmdPIDCh <- cmd.Process.Pid
			close(startedCh)
			defer close(exitedCh)

			// cmd.Wait() called inside libexec does not return until stdin, stdout and stderr are fully processed,
			// that's why cmd.Process.Wait is used inside the implementation below. Otherwise, cmd never exits.
			_, err := cmd.Process.Wait()
			if err != nil && ctx.Err() != nil {
				return errors.WithStack(cmdError{Err: err, Debug: cmd.String()})
			}
			return errors.WithStack(ctx.Err())
		})
		spawn("isolator.watchdog", parallel.Fail, func(ctx context.Context) error {
			<-ctx.Done()

			_ = inPipe.Close()
			timeout := time.After(time.Minute)

			select {
			case <-startedCh:
				for {
					select {
					case <-exitedCh:
						return errors.WithStack(ctx.Err())
					case <-timeout:
						_ = cmd.Process.Signal(unix.SIGKILL)
					case <-time.After(100 * time.Millisecond):
						_ = cmd.Process.Signal(unix.SIGTERM)
						_ = cmd.Process.Signal(unix.SIGINT)
					}
				}
			default:
				return errors.WithStack(ctx.Err())
			}
		})
		spawn("isolator.sender", parallel.Fail, func(ctx context.Context) (retErr error) {
			log := logger.Get(ctx)
			encode := wire.NewEncoder(inPipe)
			var serverStarted bool
			for {
				var content interface{}
				var ok bool

				select {
				case <-ctx.Done():
					return errors.WithStack(ctx.Err())
				case content, ok = <-outgoing:
				}

				if !ok {
					return errors.WithStack(ctx.Err())
				}

				//nolint:nestif
				if !serverStarted {
					serverStarted = true
					close(startCh)

					if config.Executor.IP != nil {
						select {
						case <-ctx.Done():
							return errors.WithStack(ctx.Err())
						case pid := <-cmdPIDCh:
							exposedPorts := make([]network.ExposedPort, 0, len(config.ExposedPorts))
							for _, p := range config.ExposedPorts {
								exposedPorts = append(exposedPorts, network.ExposedPort{
									Protocol:     p.Protocol,
									ExternalIP:   p.ExternalIP,
									ExternalPort: p.ExternalPort,
									InternalPort: p.InternalPort,
									Public:       p.Public,
								})
							}

							clean, err := network.Join(config.Executor.IP, exposedPorts, pid)
							if err != nil {
								return err
							}
							defer func() {
								if err := clean(); err != nil {
									if retErr == nil {
										retErr = err
									}
									log.Error("Cleaning network setup failed", zap.Error(err))
								}
							}()
						}
					}

					// config is guaranteed to be the first message sent
					err := encode(config.Executor)
					if err != nil {
						if errors.Is(err, io.ErrClosedPipe) {
							return errors.WithStack(ctx.Err())
						}
						return errors.WithStack(err)
					}
				}

				err = encode(content)
				if err != nil {
					if errors.Is(err, io.ErrClosedPipe) {
						return errors.WithStack(ctx.Err())
					}
					return errors.WithStack(err)
				}
			}
		})
		spawn("isolator.receiver", parallel.Fail, func(ctx context.Context) error {
			defer close(incoming)

			decode := wire.NewDecoder(outPipe, config.Types)
			for {
				content, err := decode()
				if err != nil {
					if errors.Is(err, io.EOF) {
						return errors.WithStack(ctx.Err())
					}
					return errors.WithStack(err)
				}

				incoming <- content
			}
		})
		spawn("isolator.client", parallel.Exit, func(ctx context.Context) error {
			defer close(outgoing)
			return clientFunc(ctx, incoming, outgoing)
		})

		return nil
	})
}

func sanitizeConfig(config Config) (Config, error) {
	if config.ExecutorArg == "" {
		config.ExecutorArg = executor.DefaultArg
	}
	for i, m := range config.Executor.Mounts {
		var err error
		config.Executor.Mounts[i].Host, err = filepath.Abs(m.Host)
		if err != nil {
			return Config{}, err
		}
	}
	return config, nil
}

func newExecutorServerCommand(config Config) *exec.Cmd {
	cmd := exec.Command("/proc/self/exe", config.ExecutorArg, filepath.Base(config.Dir))
	cmd.Dir = filepath.Dir(config.Dir)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/bin"}
	cmd.SysProcAttr = &unix.SysProcAttr{
		Pdeathsig: unix.SIGKILL,
		Cloneflags: unix.CLONE_NEWPID |
			unix.CLONE_NEWNS |
			unix.CLONE_NEWUSER |
			unix.CLONE_NEWIPC |
			unix.CLONE_NEWUTS |
			unix.CLONE_NEWCGROUP,
		AmbientCaps: []uintptr{
			unix.CAP_SYS_ADMIN, // by adding CAP_SYS_ADMIN executor may mount /proc
		},
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

	if !config.Executor.UseHostNetwork {
		cmd.SysProcAttr.Cloneflags |= unix.CLONE_NEWNET
	}

	return cmd
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

type cmdError struct {
	Err   error
	Debug string
}

// Error returns the string representation of an Error.
func (e cmdError) Error() string {
	return fmt.Sprintf("%s: %q", e.Err, e.Debug)
}
