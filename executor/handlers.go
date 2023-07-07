package executor

import (
	"context"
	"os/exec"

	"github.com/outofforest/libexec"
	"github.com/outofforest/logger"
	"github.com/pkg/errors"

	"github.com/outofforest/isolator/lib/docker"
	"github.com/outofforest/isolator/lib/libhttp"
	"github.com/outofforest/isolator/wire"
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

	stdOut := &logTransmitter{
		Stream: wire.StreamOut,
		Encode: encode,
	}
	stdErr := &logTransmitter{
		Stream: wire.StreamErr,
		Encode: encode,
	}

	defer func() {
		_ = stdOut.Flush()
		_ = stdErr.Flush()
	}()

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

// ExecuteHandler is a standard handler handling Execute command.
func ExecuteHandler(ctx context.Context, content interface{}, encode wire.EncoderFunc) error {
	m, ok := content.(wire.Execute)
	if !ok {
		return errors.Errorf("unexpected type %T", content)
	}

	outTransmitter := &logTransmitter{
		Stream: wire.StreamOut,
		Encode: encode,
	}
	errTransmitter := &logTransmitter{
		Stream: wire.StreamErr,
		Encode: encode,
	}

	cmd := exec.Command("/bin/sh", "-c", m.Command)
	cmd.Stdout = outTransmitter
	cmd.Stderr = errTransmitter

	logger.Get(ctx).Info("Executing command")

	err := libexec.Exec(ctx, cmd)

	_ = outTransmitter.Flush()
	_ = errTransmitter.Flush()

	return err
}

type logTransmitter struct {
	Encode wire.EncoderFunc
	Stream wire.Stream

	buf []byte
}

func (lt *logTransmitter) Write(data []byte) (int, error) {
	length := len(lt.buf) + len(data)
	if length < 100 {
		lt.buf = append(lt.buf, data...)
		return len(data), nil
	}
	buf := make([]byte, length)
	copy(buf, lt.buf)
	copy(buf[len(lt.buf):], data)
	err := lt.Encode(wire.Log{Stream: lt.Stream, Text: string(buf)})
	if err != nil {
		return 0, err
	}
	lt.buf = make([]byte, 0, len(lt.buf))
	return len(data), nil
}

func (lt *logTransmitter) Flush() error {
	if len(lt.buf) == 0 {
		return nil
	}
	if err := lt.Encode(wire.Log{Stream: lt.Stream, Text: string(lt.buf)}); err != nil {
		return err
	}
	lt.buf = make([]byte, 0, len(lt.buf))
	return nil
}
