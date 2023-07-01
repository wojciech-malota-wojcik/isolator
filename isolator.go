package isolator

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/outofforest/libexec"
	"github.com/outofforest/parallel"
	"github.com/pkg/errors"
	"github.com/ridge/must"

	"github.com/outofforest/isolator/executor"
	"github.com/outofforest/isolator/wire"
)

const capSysAdmin = 21

// Run runs executor server and communication channel.
func Run(ctx context.Context, config Config) error {
	config, err := sanitizeConfig(config)
	if err != nil {
		return err
	}

	outPipe := newPipe()
	inPipe := newPipe()

	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		startCh := make(chan struct{})

		spawn("server", parallel.Fail, func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return errors.WithStack(err)
			case <-startCh:
			}

			cmd := newExecutorServerCommand(config)
			cmd.Stdout = outPipe
			cmd.Stdin = inPipe

			return libexec.Exec(ctx, cmd)
		})
		spawn("watchdog", parallel.Fail, func(ctx context.Context) error {
			<-ctx.Done()

			// cmd.Wait() called inside libexec does not return until stdin, stdout and stderr are fully processed,
			// so they must be closed here. Otherwise, cmd never exits.
			_ = outPipe.Close()
			_ = inPipe.Close()

			return errors.WithStack(ctx.Err())
		})
		spawn("client", parallel.Fail, func(ctx context.Context) error {
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
						case content, ok = <-config.Outgoing:
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
					defer close(config.Incoming)

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
						case config.Incoming <- content:
						}
					}
				})

				return nil
			})
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
	cmd := exec.Command(must.String(filepath.Abs(must.String(filepath.EvalSymlinks(must.String(os.Executable()))))), config.ExecutorArg)
	cmd.Dir = config.Dir
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig:  syscall.SIGKILL,
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
