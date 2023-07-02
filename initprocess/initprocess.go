package initprocess

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

const (
	procFSPath     = "/proc"
	pidIndex       = 0
	commandIndex   = 1
	stateIndex     = 2
	parentPIDIndex = 3
	zombieState    = "Z"
)

var (
	pid        = fmt.Sprintf("%d", os.Getpid())
	procRegExp = regexp.MustCompile("^[0-9]+$")
)

// Flavour adds logic required by the init process. It awaits zombie processes and terminates all the child processes on exit.
func Flavour(ctx context.Context, appFunc parallel.Task) error {
	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		appTerminatedCh := make(chan struct{})

		spawn("", parallel.Exit, func(ctx context.Context) error {
			defer close(appTerminatedCh)

			return appFunc(ctx)
		})
		spawn("init", parallel.Fail, func(ctx context.Context) error {
			log := logger.Get(ctx)

			defer func() {
				// terminating all the processes may start after exit of the main logic, so we are sure
				// that no new process is started by the app.
				<-appTerminatedCh

				runningErr := errors.New("children processes are still running")
				timeout := time.After(time.Minute)
				for {
					err := func() error {
						children, err := subProcesses()
						if err != nil {
							return err
						}

						log.Info("Terminating leftover processes", zap.Int("count", len(children)))

						var running uint32
						if len(children) > 0 {
							for _, properties := range children {
								childPID, err := strconv.Atoi(properties[pidIndex])
								if err != nil {
									return errors.WithStack(err)
								}

								proc, err := os.FindProcess(childPID)
								if err != nil {
									return errors.WithStack(err)
								}

								log := log.With(zap.Int("pid", childPID), zap.String("command", properties[commandIndex]))
								if properties[stateIndex] == zombieState {
									if _, err := proc.Wait(); err != nil {
										return errors.WithStack(err)
									}
									log.Info("Zombie process deleted")
									continue
								}

								running++
								select {
								case <-timeout:
									log.Error("Killing process")
									if err := proc.Signal(syscall.SIGKILL); err != nil {
										return err
									}
								default:
									log.Warn("Terminating process")
									if err := proc.Signal(syscall.SIGTERM); err != nil {
										return err
									}
									if err := proc.Signal(syscall.SIGINT); err != nil {
										return err
									}
								}
							}
							return runningErr
						}
						return nil
					}()

					switch {
					case err == nil:
						log.Info("No more processes running. Exiting.")
						return
					case errors.Is(err, runningErr):
					default:
						log.Error("Error while terminating processes", zap.Error(err))
					}

					<-time.After(time.Second)
				}
			}()

			var zombies map[int]string

			for {
				select {
				case <-ctx.Done():
					return errors.WithStack(ctx.Err())
				case <-time.After(30 * time.Second):
				}

				for zPID, command := range zombies {
					proc, err := os.FindProcess(zPID)
					if err != nil {
						return errors.WithStack(err)
					}
					if _, err := proc.Wait(); err != nil {
						return errors.WithStack(err)
					}

					log.Info("Zombie process deleted", zap.Int("pid", zPID), zap.String("command", command))
				}

				zombies = map[int]string{}

				children, err := subProcesses()
				if err != nil {
					return err
				}
				for _, properties := range children {
					if properties[stateIndex] != zombieState {
						continue
					}

					zombiePID, err := strconv.Atoi(properties[pidIndex])
					if err != nil {
						return errors.WithStack(err)
					}

					zombies[zombiePID] = properties[commandIndex]
				}
			}
		})

		return nil
	})
}

func subProcesses() ([][]string, error) {
	procs, err := os.ReadDir(procFSPath)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	result := [][]string{}
	for _, procDir := range procs {
		if !procDir.IsDir() || !procRegExp.MatchString(procDir.Name()) {
			continue
		}

		statPath := filepath.Join(procFSPath, procDir.Name(), "stat")
		statRaw, err := os.ReadFile(statPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, errors.WithStack(err)
		}

		properties := strings.SplitN(string(statRaw), " ", parentPIDIndex+2)
		if properties[parentPIDIndex] != pid {
			continue
		}

		if err != nil {
			return nil, errors.WithStack(err)
		}

		result = append(result, properties)
	}

	return result, nil
}
