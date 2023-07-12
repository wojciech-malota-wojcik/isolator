package scenarios

import (
	"context"
	"net"
	"os"
	"path/filepath"

	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/outofforest/isolator"
	"github.com/outofforest/isolator/lib/task"
	"github.com/outofforest/isolator/network"
	"github.com/outofforest/isolator/wire"
)

// Container defines container.
type Container struct {
	// Name is the name of the container.
	Name string

	// Image is the name of the image.
	Image string

	// Tag is the tag of the image.
	Tag string

	// EnvVars sets environment variables inside container.
	EnvVars map[string]string

	// User is the username or UID to use.
	User string

	// Mounts are the mounts to configure inside container.
	Mounts []Mount

	// WorkingDir specifies a path to working directory.
	WorkingDir string

	// Entrypoint sets entrypoint for container.
	Entrypoint []string

	// Args sets args for container.
	Args []string

	// IP is the IP address of the container.
	IP *net.IPNet

	// DNS is the list of nameservers to configure inside container.
	DNS []net.IP

	// Hosts is the list of hosts and their IP addresses to resolve inside namespace.
	Hosts map[string]net.IP

	// ExposedPorts is the list of ports to expose.
	ExposedPorts []ExposedPort
}

// GetName returns the name of the container.
func (c Container) GetName() string {
	return c.Name
}

// GetIP returns IP of the container.
func (c Container) GetIP() net.IP {
	if c.IP == nil {
		return nil
	}
	return c.IP.IP
}

// GetTaskFunc returns task function running the container.
func (c Container) GetTaskFunc(config RunAppsConfig, appHosts map[string]net.IP, spawn parallel.SpawnFn) task.Func {
	return func(ctx context.Context) error {
		ctx = logger.With(ctx, zap.String("appName", c.Name))

		if err := os.MkdirAll(config.CacheDir, 0o700); err != nil {
			return errors.WithStack(err)
		}

		if err := os.MkdirAll(config.AppsDir, 0o700); err != nil {
			return errors.WithStack(err)
		}
		appDir := filepath.Join(config.AppsDir, c.Name)
		// 0o755 mode is essential here. Without this, container running as non-root user will
		// fail with "permission denied"
		if err := os.Mkdir(appDir, 0o755); err != nil {
			return errors.WithStack(err)
		}

		if err := c.inflate(ctx, config, appDir); err != nil {
			return err
		}

		spawn(c.Name, parallel.Fail, func(ctx context.Context) error {
			ctx = logger.With(ctx, zap.String("appName", c.Name))
			return c.run(ctx, config, appDir, appHosts)
		})
		return nil
	}
}

func (c Container) inflate(ctx context.Context, config RunAppsConfig, appDir string) (retErr error) {
	ctx = logger.With(ctx, zap.String("container", c.Name))
	log := logger.Get(ctx)

	inflateNetwork, clean, err := network.Random(30)
	if err != nil {
		return err
	}
	defer func() {
		if err := clean(); err != nil {
			if retErr == nil {
				retErr = err
			}
			log.Error("Cleaning network failed", zap.Error(err))
		}
	}()

	return isolator.Run(ctx, isolator.Config{
		Dir: appDir,
		Types: []interface{}{
			wire.Result{},
		},
		Executor: wire.Config{
			IP:       network.Addr(inflateNetwork, 2),
			Hostname: "inflate",
			Mounts: []wire.Mount{
				{
					Host:      config.CacheDir,
					Namespace: "/.cache",
					Writable:  true,
				},
			},
		},
	}, func(ctx context.Context, incoming <-chan interface{}, outgoing chan<- interface{}) error {
		log := logger.Get(ctx)
		log.Info("Inflating container's filesystem")

		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		case outgoing <- wire.InflateDockerImage{
			CacheDir: "/.cache",
			Image:    c.Image,
			Tag:      c.Tag,
		}:
		}

		for content := range incoming {
			switch m := content.(type) {
			// wire.Result means command finished
			case wire.Result:
				if m.Error != "" {
					return errors.Errorf("inflating image failed: %s", m.Error)
				}
				log.Info("Container's filesystem inflated")
				return errors.WithStack(ctx.Err())
			default:
				return errors.Errorf("unexpected message %T received", content)
			}
		}

		return errors.WithStack(ctx.Err())
	})
}

func (c Container) run(ctx context.Context, config RunAppsConfig, appDir string, appHosts map[string]net.IP) error {
	hosts := map[string]net.IP{}
	for h, ip := range c.Hosts {
		hosts[h] = ip
	}
	for h, ip := range appHosts {
		hosts[h] = ip
	}

	runConfig := isolator.Config{
		Dir: appDir,
		Types: []interface{}{
			wire.Log{},
			wire.Result{},
		},
		Executor: wire.Config{
			IP:              c.IP,
			Hostname:        c.Name,
			DNS:             c.DNS,
			Hosts:           hosts,
			ConfigureSystem: true,
			Mounts: []wire.Mount{
				{
					Host:      config.CacheDir,
					Namespace: "/.cache",
					Writable:  true,
				},
			},
		},
	}

	for _, p := range c.ExposedPorts {
		runConfig.ExposedPorts = append(runConfig.ExposedPorts, isolator.ExposedPort{
			Protocol:     p.Protocol,
			ExternalIP:   p.HostIP,
			ExternalPort: p.HostPort,
			InternalPort: p.NamespacePort,
			Public:       p.Public,
		})
	}

	for _, m := range c.Mounts {
		runConfig.Executor.Mounts = append(runConfig.Executor.Mounts, wire.Mount{
			Host:      m.Host,
			Namespace: m.Namespace,
			Writable:  m.Writable,
		})
	}

	return isolator.Run(ctx, runConfig, func(ctx context.Context, incoming <-chan interface{}, outgoing chan<- interface{}) error {
		log := logger.Get(ctx)
		log.Info("Starting container")

		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		case outgoing <- wire.RunDockerContainer{
			CacheDir:   "/.cache",
			Name:       c.Name,
			Image:      c.Image,
			Tag:        c.Tag,
			EnvVars:    c.EnvVars,
			User:       c.User,
			WorkingDir: c.WorkingDir,
			Entrypoint: c.Entrypoint,
			Args:       c.Args,
		}:
		}

		for content := range incoming {
			switch m := content.(type) {
			// wire.Log contains message printed by executed command to stdout or stderr
			case wire.Log:
				stream, err := wire.ToStream(m.Stream)
				if err != nil {
					return errors.WithStack(err)
				}
				if _, err := stream.WriteString(m.Text); err != nil {
					return errors.WithStack(err)
				}
			// wire.Result means command finished
			case wire.Result:
				if m.Error != "" {
					return errors.Errorf("container failed: %s", m.Error)
				}
				return errors.WithStack(ctx.Err())
			default:
				return errors.Errorf("unexpected message %T received", content)
			}
		}

		return errors.WithStack(ctx.Err())
	})
}
