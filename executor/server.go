package executor

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
	"github.com/outofforest/run"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/outofforest/isolator/initprocess"
	"github.com/outofforest/isolator/network"
	"github.com/outofforest/isolator/wire"
)

func runServer(ctx context.Context, config Config, rootDir string) error {
	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		log := logger.Get(ctx)
		log.Info("Server started")

		spawn("watchdog", parallel.Fail, func(ctx context.Context) error {
			<-ctx.Done()
			// os.Stdin is used as input stream for client, so it has to be closed to force client.Receive() to exit
			_ = os.Stdin.Close()
			return errors.WithStack(ctx.Err())
		})
		spawn("server", parallel.Fail, func(ctx context.Context) error {
			decode := wire.NewDecoder(os.Stdin, append(config.Router.Types(), wire.Config{}))
			encode := wire.NewEncoder(os.Stdout)

			content, err := decode()
			if err != nil {
				return err
			}
			runtimeConfig, ok := content.(wire.Config)
			if !ok {
				return errors.Errorf("expected Config message but got: %T", content)
			}

			if err := prepareNewRoot(rootDir); err != nil {
				return err
			}

			if runtimeConfig.ConfigureSystem {
				if err := mountProc("proc"); err != nil {
					return err
				}
				if err := mountTmp(); err != nil {
					return err
				}
				if err := populateDev(); err != nil {
					return err
				}
				if err := configureDNS(runtimeConfig.DNS); err != nil {
					return err
				}
				if err := configureHosts(runtimeConfig.Hosts, runtimeConfig.Hostname, runtimeConfig.IP); err != nil {
					return err
				}
			} else {
				if err := mountProc(".proc"); err != nil {
					return err
				}
			}

			if err := applyMounts(runtimeConfig.Mounts); err != nil {
				return err
			}

			if err := pivotRoot(); err != nil {
				return err
			}

			if runtimeConfig.Hostname != "" {
				if err := syscall.Sethostname([]byte(runtimeConfig.Hostname)); err != nil {
					return errors.WithStack(err)
				}
			}

			if runtimeConfig.IP != nil {
				if err := network.SetupContainer(runtimeConfig.IP); err != nil {
					return err
				}
			}

			return run.WithFlavours(ctx, []run.FlavourFunc{
				initprocess.Flavour,
			}, func(ctx context.Context) error {
				log := logger.Get(ctx)

				for {
					content, err := decode()
					if err != nil {
						if ctx.Err() != nil || errors.Is(err, io.EOF) {
							return errors.WithStack(ctx.Err())
						}
						return err
					}

					handler, err := config.Router.Handler(content)
					if err != nil {
						return err
					}

					var errStr string
					if err := handler(ctx, content, encode); err != nil {
						log.Error("Command returned error", zap.Any("content", content), zap.Error(err))
						errStr = err.Error()
					}
					if err := encode(wire.Result{
						Error: errStr,
					}); err != nil {
						return err
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
		return errors.WithStack(err)
	}

	// PivotRoot requires new root to be on different mountpoint, so let's bind it to itself
	if err := syscall.Mount(rootDir, rootDir, "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
		return errors.WithStack(err)
	}

	// Let's assume that new filesystem is the current working dir even before pivoting to make life easier
	return errors.WithStack(os.Chdir(rootDir))
}

func mountProc(target string) error {
	if err := os.Mkdir(target, 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	if err := syscall.Mount("none", target, "proc", 0, ""); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func mountTmp() error {
	const targetDir = "tmp"

	if err := os.Mkdir(targetDir, 0o777|os.ModeSticky); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	if err := syscall.Mount("none", targetDir, "tmpfs", 0, ""); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func populateDev() error {
	devDir := "dev"
	if err := os.Mkdir(devDir, 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	if err := syscall.Mount("none", devDir, "tmpfs", 0, ""); err != nil {
		return errors.WithStack(err)
	}
	for _, dev := range []string{"console", "null", "zero", "random", "urandom"} {
		devPath := filepath.Join(devDir, dev)

		f, err := os.OpenFile(devPath, os.O_CREATE|os.O_RDONLY, 0o644)
		if err != nil {
			return errors.WithStack(err)
		}
		if err := f.Close(); err != nil {
			return errors.WithStack(err)
		}
		if err := syscall.Mount(filepath.Join("/", devPath), devPath, "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
			return errors.WithStack(err)
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

		info, err := os.Stat(m.Host)
		if err != nil {
			return errors.WithStack(err)
		}
		if info.IsDir() {
			if err := os.MkdirAll(m.Container, 0o700); err != nil {
				return errors.WithStack(err)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(m.Container), 0o700); err != nil {
				return errors.WithStack(err)
			}
			err := func() error {
				f, err := os.OpenFile(m.Container, os.O_CREATE|os.O_WRONLY|os.O_EXCL, info.Mode())
				if err != nil {
					if os.IsExist(err) {
						return nil
					}
					return errors.WithStack(err)
				}
				return errors.WithStack(f.Close())
			}()
			if err != nil {
				return err
			}
		}
		if err := syscall.Mount(m.Host, m.Container, "", syscall.MS_BIND|syscall.MS_PRIVATE, ""); err != nil {
			return errors.WithStack(err)
		}
		if !m.Writable {
			if err := syscall.Mount(m.Host, m.Container, "", syscall.MS_BIND|syscall.MS_PRIVATE|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				return errors.WithStack(err)
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
		return errors.WithStack(err)
	}
	if err := syscall.Mount("", ".old", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return errors.WithStack(err)
	}
	if err := syscall.Unmount(".old", syscall.MNT_DETACH); err != nil {
		return errors.WithStack(err)
	}
	return errors.WithStack(os.Remove(".old"))
}

func configureDNS(dns []net.IP) error {
	if dns == nil {
		dns = []net.IP{
			net.IPv4(8, 8, 8, 8),
			net.IPv4(1, 1, 1, 1),
		}
	}

	if err := os.Mkdir("etc", 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}

	f, err := os.OpenFile(filepath.Join("etc", "resolv.conf"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return errors.WithStack(err)
	}
	defer f.Close()

	for _, s := range dns {
		if _, err := f.WriteString(fmt.Sprintf("nameserver %s\n", s)); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

func configureHosts(hosts map[string]net.IP, name string, ip *net.IPNet) error {
	if hosts == nil {
		hosts = map[string]net.IP{}
	}
	hosts["localhost"] = net.IPv4(127, 0, 0, 1)
	if name != "" {
		hosts[name] = ip.IP
	}

	if err := os.Mkdir("etc", 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}

	f, err := os.OpenFile(filepath.Join("etc", "hosts"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return errors.WithStack(err)
	}
	defer f.Close()

	for h, ip := range hosts {
		if _, err := f.WriteString(fmt.Sprintf("%s %s\n", ip, h)); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}
