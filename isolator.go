package isolator

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"github.com/outofforest/isolator/executor"
	"github.com/outofforest/isolator/wire"
)

// ClientFunc defines the client function for isolator.
type ClientFunc func(ctx context.Context, incoming <-chan interface{}, outgoing chan<- interface{}) error

// Run runs executor server and communication channel.
func Run(ctx context.Context, config Config, clientFunc ClientFunc) error {
	config, err := sanitizeConfig(config)
	if err != nil {
		return err
	}

	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		startCh := make(chan struct{})

		outPipe := newPipe()
		inPipe := newPipe()

		incoming := make(chan interface{})
		outgoing := make(chan interface{})

		spawn("server", parallel.Fail, func(ctx context.Context) error {
			defer func() {
				_ = outPipe.Close()
				_ = inPipe.Close()
			}()

			select {
			case <-ctx.Done():
				return errors.WithStack(ctx.Err())
			case <-startCh:
			}

			if err := os.MkdirAll(config.Dir, 0o700); err != nil {
				return errors.WithStack(err)
			}

			cmd := newExecutorServerCommand(config)
			cmd.Stdout = outPipe
			cmd.Stdin = inPipe

			// cmd.Wait() called inside libexec does not return until stdin, stdout and stderr are fully processed,
			// that's why cmd.Process.Wait is used inside the implementation below. Otherwise, cmd never exits.
			return execServer(ctx, cmd)
		})
		spawn("watchdog", parallel.Fail, func(ctx context.Context) error {
			<-ctx.Done()

			// cmd.Wait() called inside libexec does not return until stdin, stdout and stderr are fully processed,
			// so they must be closed here. Otherwise, cmd never exits.
			_ = outPipe.Close()
			_ = inPipe.Close()

			return errors.WithStack(ctx.Err())
		})
		spawn("communication", parallel.Fail, func(ctx context.Context) error {
			return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
				spawn("sender", parallel.Fail, func(ctx context.Context) error {
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

						if !serverStarted {
							serverStarted = true
							close(startCh)

							// config is guaranteed to be the first message sent
							err := encode(config.Executor)
							if err != nil {
								if errors.Is(err, io.EOF) {
									return errors.WithStack(ctx.Err())
								}
								return errors.WithStack(err)
							}
						}

						err = encode(content)
						if err != nil {
							if errors.Is(err, io.EOF) {
								return errors.WithStack(ctx.Err())
							}
							return errors.WithStack(err)
						}
					}
				})
				spawn("receiver", parallel.Fail, func(ctx context.Context) error {
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

						select {
						case <-ctx.Done():
							return errors.WithStack(ctx.Err())
						case incoming <- content:
						}
					}
				})

				return nil
			})
		})
		spawn("client", parallel.Exit, func(ctx context.Context) error {
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
		Pdeathsig:  unix.SIGKILL,
		Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS | unix.CLONE_NEWUSER | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP,
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

func execServer(ctx context.Context, cmds ...*exec.Cmd) error {
	for _, cmd := range cmds {
		cmd := cmd
		if cmd.Stdout == nil {
			cmd.Stdout = os.Stdout
		}
		if cmd.Stderr == nil {
			cmd.Stderr = os.Stderr
		}
		if cmd.Stdin == nil {
			// If Stdin is nil, then exec library tries to assign it to /dev/null
			// Null device does not exist in chrooted environment unless created, so we set a fake nil buffer
			// just to remove this dependency
			cmd.Stdin = bytes.NewReader(nil)
		}

		logger.Get(ctx).Debug("Executing command", zap.Stringer("command", cmd))

		if err := cmd.Start(); err != nil {
			return errors.WithStack(err)
		}

		err := parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
			spawn("cmd", parallel.Exit, func(ctx context.Context) error {
				_, err := cmd.Process.Wait()
				if ctx.Err() != nil {
					return errors.WithStack(ctx.Err())
				}
				if err != nil {
					return errors.WithStack(cmdError{Err: err, Debug: cmd.String()})
				}
				return nil
			})
			spawn("ctx", parallel.Fail, func(ctx context.Context) error {
				<-ctx.Done()
				_ = cmd.Process.Signal(syscall.SIGTERM)
				_ = cmd.Process.Signal(syscall.SIGINT)
				return errors.WithStack(ctx.Err())
			})
			return nil
		})
		if err != nil {
			return err
		}
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
