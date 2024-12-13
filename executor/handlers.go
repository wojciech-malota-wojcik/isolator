package executor

import (
	"context"
	"os/exec"

	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/outofforest/isolator/lib/docker"
	"github.com/outofforest/isolator/lib/libhttp"
	"github.com/outofforest/isolator/wire"
	"github.com/outofforest/libexec"
	"github.com/outofforest/logger"
)

// NewInflateDockerImageHandler creates new standard handler for InflateDockerImage command.
func NewInflateDockerImageHandler() HandlerFunc {
	// creating http client before pivoting/chrooting because client reads CA certificates from system pool
	httpClient := libhttp.NewSelfClient()

	return func(ctx context.Context, content interface{}, encode wire.EncoderFunc) error {
		m, ok := content.(wire.InflateDockerImage)
		if !ok {
			return errors.Errorf("unexpected type %T", content)
		}
		return docker.InflateImage(ctx, docker.InflateImageConfig{
			HTTPClient: httpClient,
			CacheDir:   m.CacheDir,
			Image:      m.Image,
			Tag:        m.Tag,
		})
	}
}

// RunDockerContainerHandler is a standard handler for RunDockerContainer command.
func RunDockerContainerHandler(ctx context.Context, content interface{}, encode wire.EncoderFunc) error {
	m, ok := content.(wire.RunDockerContainer)
	if !ok {
		return errors.Errorf("unexpected type %T", content)
	}

	stdOut := newLogTransmitter(encode)
	stdErr := newLogTransmitter(encode)

	return docker.RunContainer(ctx, docker.RunContainerConfig{
		CacheDir:   m.CacheDir,
		Image:      m.Image,
		Tag:        m.Tag,
		Name:       m.Name,
		EnvVars:    m.EnvVars,
		User:       m.User,
		WorkingDir: m.WorkingDir,
		Entrypoint: m.Entrypoint,
		Args:       m.Args,

		StdOut: stdOut,
		StdErr: stdErr,
	})
}

// EmbeddedFunc defines embedded function.
type EmbeddedFunc func(ctx context.Context, args []string) error

// NewRunEmbeddedFunctionHandler returns new handler running embedded function.
func NewRunEmbeddedFunctionHandler(funcs map[string]EmbeddedFunc) HandlerFunc {
	return func(ctx context.Context, content interface{}, encode wire.EncoderFunc) error {
		m, ok := content.(wire.RunEmbeddedFunction)
		if !ok {
			return errors.Errorf("unexpected type %T", content)
		}

		fn, exists := funcs[m.Name]
		if !exists {
			return errors.Errorf("embedded function %s does not exist", m.Name)
		}

		log := logger.Get(ctx)
		log.Info("Starting embedded function")

		if err := fn(withZAPTransmitter(ctx, encode), m.Args); err != nil {
			log.Error("Embedded function exited with error", zap.Error(err))
			return err
		}

		log.Info("Embedded function exited")
		return nil
	}
}

// ExecuteHandler is a standard handler handling Execute command.
func ExecuteHandler(ctx context.Context, content interface{}, encode wire.EncoderFunc) error {
	m, ok := content.(wire.Execute)
	if !ok {
		return errors.Errorf("unexpected type %T", content)
	}

	outTransmitter := newLogTransmitter(encode)
	errTransmitter := newLogTransmitter(encode)

	cmd := exec.Command("/bin/sh", "-c", m.Command)
	cmd.Stdout = outTransmitter
	cmd.Stderr = errTransmitter

	log := logger.Get(ctx)
	log.Info("Starting command")

	if err := libexec.Exec(ctx, cmd); err != nil {
		log.Error("Command exited with error", zap.Error(err))
		return err
	}

	log.Info("Command exited")
	return nil
}
