package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
	"github.com/outofforest/run"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/outofforest/isolator/initprocess"
	"github.com/outofforest/isolator/wire"
)

func runServer(ctx context.Context, config Config, rootDir string) error {
	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		spawn("watchdog", parallel.Fail, func(ctx context.Context) error {
			<-ctx.Done()
			// os.Stdin is used as input stream for client, so it has to be closed to force client.Receive() to exit
			_ = os.Stdin.Close()
			return errors.WithStack(ctx.Err())
		})
		spawn("server", parallel.Exit, func(ctx context.Context) error {
			decode := wire.NewDecoder(os.Stdin, append(config.Router.Types(), wire.Config{}))
			encode := wire.NewEncoder(os.Stdout)

			content, err := decode()
			if err != nil {
				return errors.WithStack(fmt.Errorf("fetching configuration failed: %w", err))
			}
			runtimeConfig, ok := content.(wire.Config)
			if !ok {
				return errors.Errorf("expected Config message but got: %T", content)
			}

			if err := prepareNewRoot(rootDir); err != nil {
				return errors.WithStack(fmt.Errorf("preparing new root filesystem failed: %w", err))
			}

			if runtimeConfig.NoStandardMounts {
				if err := mountProc(".proc"); err != nil {
					return err
				}
			} else {
				if err := mountProc("proc"); err != nil {
					return err
				}
				if err := mountTmp(); err != nil {
					return err
				}
				if err := populateDev(); err != nil {
					return err
				}
				if err := configureDNS(); err != nil {
					return err
				}
			}

			if err := applyMounts(runtimeConfig.Mounts); err != nil {
				return errors.WithStack(fmt.Errorf("mounting host directories failed: %w", err))
			}

			if err := pivotRoot(); err != nil {
				return errors.WithStack(fmt.Errorf("pivoting root filesystem failed: %w", err))
			}

			return run.WithFlavours(ctx, []run.FlavourFunc{
				initprocess.Flavour,
			}, func(ctx context.Context) error {
				log := logger.Get(ctx)

				for {
					content, err := decode()
					if err != nil {
						if ctx.Err() != nil || errors.Is(err, io.EOF) {
							return ctx.Err()
						}
						return errors.WithStack(fmt.Errorf("receiving message failed: %w", err))
					}

					handler, err := config.Router.Handler(content)
					if err != nil {
						return err
					}

					var errStr string
					if err := handler(ctx, content, encode); err != nil {
						log.Error("Command returned error", zap.Error(err))
						errStr = err.Error()
					}
					if err := encode(wire.Result{
						Error: errStr,
					}); err != nil {
						return errors.WithStack(fmt.Errorf("command status reporting failed: %w", err))
					}
				}
			})
		})
		return nil
	})
}

func prepareNewRoot(rootDir string) error {
	// systemd remounts everything as MS_SHARED, to prevent mess let's remount everything back to MS_PRIVATE inside namespace
	if err := syscall.Mount("", "/", "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
		return errors.WithStack(fmt.Errorf("remounting / as slave failed: %w", err))
	}

	// PivotRoot requires new root to be on different mountpoint, so let's bind it to itself
	if err := syscall.Mount(rootDir, rootDir, "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
		return errors.WithStack(fmt.Errorf("binding new root to itself failed: %w", err))
	}

	// Let's assume that new filesystem is the current working dir even before pivoting to make life easier
	return errors.WithStack(os.Chdir(rootDir))
}

func mountProc(target string) error {
	if err := os.Mkdir(target, 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	if err := syscall.Mount("none", target, "proc", 0, ""); err != nil {
		return errors.WithStack(fmt.Errorf("mounting proc failed: %w", err))
	}
	return nil
}

func mountTmp() error {
	const targetDir = "tmp"

	if err := os.Mkdir(targetDir, 0o777|os.ModeSticky); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	if err := syscall.Mount("none", targetDir, "tmpfs", 0, ""); err != nil {
		return errors.WithStack(fmt.Errorf("mounting tmp failed: %w", err))
	}
	return nil
}

func populateDev() error {
	devDir := "dev"
	if err := os.Mkdir(devDir, 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	if err := syscall.Mount("none", devDir, "tmpfs", 0, ""); err != nil {
		return errors.WithStack(fmt.Errorf("mounting tmpfs for dev failed: %w", err))
	}
	for _, dev := range []string{"console", "null", "zero", "random", "urandom"} {
		devPath := filepath.Join(devDir, dev)

		f, err := os.OpenFile(devPath, os.O_CREATE|os.O_RDONLY, 0o644)
		if err != nil {
			return errors.WithStack(fmt.Errorf("creating dev/%s file failed: %w", dev, err))
		}
		if err := f.Close(); err != nil {
			return errors.WithStack(fmt.Errorf("closing dev/%s file failed: %w", dev, err))
		}
		if err := syscall.Mount(filepath.Join("/", devPath), devPath, "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
			return errors.WithStack(fmt.Errorf("binding dev/%s device failed: %w", dev, err))
		}
	}
	links := map[string]string{
		"fd":     "/proc/self/fd",
		"stdin":  "fd/0",
		"stdout": "fd/1",
		"stderr": "fd/2",
	}
	for newName, oldName := range links {
		if err := os.Symlink(oldName, devDir+"/"+newName); err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func applyMounts(mounts []wire.Mount) error {
	// To mount readonly, trick is required:
	// 1. mount dir normally
	// 2. remount it using read-only option
	for _, m := range mounts {
		// force path in container should be relative to the new filesystem to prevent hacks (we haven't pivoted yet)
		m.Container = filepath.Join(".", m.Container)
		if err := os.MkdirAll(m.Container, 0o700); err != nil && !os.IsExist(err) {
			return errors.WithStack(err)
		}
		if err := syscall.Mount(m.Host, m.Container, "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
			return errors.WithStack(fmt.Errorf("mounting %s to %s failed: %w", m.Host, m.Container, err))
		}
		if !m.Writable {
			if err := syscall.Mount(m.Host, m.Container, "", syscall.MS_BIND|syscall.MS_PRIVATE|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				return errors.WithStack(fmt.Errorf("remounting readonly %s to %s failed: %w", m.Host, m.Container, err))
			}
		}
	}
	return nil
}

func pivotRoot() error {
	if err := os.Mkdir(".old", 0o700); err != nil {
		return errors.WithStack(err)
	}
	if err := syscall.PivotRoot(".", ".old"); err != nil {
		return errors.WithStack(fmt.Errorf("pivoting / failed: %w", err))
	}
	if err := syscall.Mount("", ".old", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return errors.WithStack(fmt.Errorf("remounting .old as private failed: %w", err))
	}
	if err := syscall.Unmount(".old", syscall.MNT_DETACH); err != nil {
		return errors.WithStack(fmt.Errorf("unmounting .old failed: %w", err))
	}
	return errors.WithStack(os.Remove(".old"))
}

func configureDNS() error {
	if err := os.Mkdir("etc", 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	return errors.WithStack(os.WriteFile("etc/resolv.conf", []byte("nameserver 8.8.8.8\nnameserver 8.8.4.4\n"), 0o644))
}
