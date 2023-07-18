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
	"github.com/outofforest/isolator/wire"
)

var _ Application = Embedded{}

// Embedded defines embedded function to run inside isolation.
type Embedded struct {
	// Name is the name of the function.
	Name string

	// Mounts are the mounts to configure inside embedded function.
	Mounts []Mount

	// Args sets args for embedded function.
	Args []string

	// IP is the IP address of the embedded function.
	IP *net.IPNet

	// DNS is the list of nameservers to configure inside embedded function.
	DNS []net.IP

	// Hosts is the list of hosts and their IP addresses to resolve inside namespace.
	Hosts map[string]net.IP

	// ExposedPorts is the list of ports to expose.
	ExposedPorts []ExposedPort
}

// GetName returns the name of the function.
func (e Embedded) GetName() string {
	return e.Name
}

// GetIP returns IP of the function.
func (e Embedded) GetIP() net.IP {
	if e.IP == nil {
		return nil
	}
	return e.IP.IP
}

// GetTaskFunc returns task function running the embedded function.
func (e Embedded) GetTaskFunc(config RunAppsConfig, appHosts map[string]net.IP, spawn parallel.SpawnFn, logsCh chan<- logEnvelope) task.Func {
	return func(ctx context.Context) error {
		if err := os.MkdirAll(config.AppsDir, 0o700); err != nil {
			return errors.WithStack(err)
		}
		appDir := filepath.Join(config.AppsDir, e.Name)
		if err := os.Mkdir(appDir, 0o700); err != nil {
			return errors.WithStack(err)
		}

		spawn(e.Name, parallel.Fail, func(ctx context.Context) error {
			ctx = logger.With(ctx, zap.String("appName", e.Name), zap.Stringer("appIP", e.IP))
			return e.run(ctx, appDir, appHosts, logsCh)
		})
		return nil
	}
}

func (e Embedded) run(ctx context.Context, appDir string, appHosts map[string]net.IP, logsCh chan<- logEnvelope) error {
	hosts := map[string]net.IP{}
	for h, ip := range e.Hosts {
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
			IP:              e.IP,
			Hostname:        e.Name,
			DNS:             e.DNS,
			Hosts:           hosts,
			ConfigureSystem: true,
		},
	}

	for _, p := range e.ExposedPorts {
		runConfig.ExposedPorts = append(runConfig.ExposedPorts, isolator.ExposedPort{
			Protocol:     p.Protocol,
			ExternalIP:   p.HostIP,
			ExternalPort: p.HostPort,
			InternalPort: p.NamespacePort,
			Public:       p.Public,
		})
	}

	for _, m := range e.Mounts {
		runConfig.Executor.Mounts = append(runConfig.Executor.Mounts, wire.Mount{
			Host:      m.Host,
			Namespace: m.Namespace,
			Writable:  m.Writable,
		})
	}

	return isolator.Run(ctx, runConfig, func(ctx context.Context, incoming <-chan interface{}, outgoing chan<- interface{}) error {
		log := logger.Get(ctx)
		log.Info("Starting embedded")

		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		case outgoing <- wire.RunEmbeddedFunction{
			Name: e.Name,
			Args: e.Args,
		}:
		}

		for content := range incoming {
			switch m := content.(type) {
			// wire.Log contains message printed by executed command to stdout or stderr
			case wire.Log:
				logsCh <- logEnvelope{AppName: e.Name, Log: m}
			// wire.Result means command finished
			case wire.Result:
				if m.Error != "" {
					return errors.Errorf("embedded failed: %s", m.Error)
				}
				return errors.WithStack(ctx.Err())
			default:
				return errors.Errorf("unexpected message %T received", content)
			}
		}

		return errors.WithStack(ctx.Err())
	})
}
