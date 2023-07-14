package executor

import (
	"bytes"
	"context"
	"os/exec"

	"github.com/outofforest/libexec"
	"github.com/outofforest/logger"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

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

	stdOut := newLogTransmitter(encode, wire.StreamOut)
	stdErr := newLogTransmitter(encode, wire.StreamErr)

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

		return fn(zapTransmitter(ctx, encode), m.Args)
	}
}

// ExecuteHandler is a standard handler handling Execute command.
func ExecuteHandler(ctx context.Context, content interface{}, encode wire.EncoderFunc) error {
	m, ok := content.(wire.Execute)
	if !ok {
		return errors.Errorf("unexpected type %T", content)
	}

	outTransmitter := newLogTransmitter(encode, wire.StreamOut)
	errTransmitter := newLogTransmitter(encode, wire.StreamErr)

	cmd := exec.Command("/bin/sh", "-c", m.Command)
	cmd.Stdout = outTransmitter
	cmd.Stderr = errTransmitter

	logger.Get(ctx).Info("Executing command")

	err := libexec.Exec(ctx, cmd)

	return err
}

func newLogTransmitter(encode wire.EncoderFunc, stream wire.Stream) *logTransmitter {
	return &logTransmitter{
		encode: encode,
		stream: stream,
	}
}

type logTransmitter struct {
	encode wire.EncoderFunc
	stream wire.Stream
	buf    []byte
	start  int
	length int
}

func (lt *logTransmitter) Write(data []byte) (int, error) {
	dataLength := len(data)
	if dataLength == 0 {
		return 0, nil
	}

	if len(lt.buf[lt.start+lt.length:]) < dataLength {
		newBuf := make([]byte, lt.length+dataLength)
		copy(newBuf, lt.buf[lt.start:lt.length])
		lt.buf = newBuf
		lt.start = 0
	}
	copy(lt.buf[lt.start+lt.length:], data)
	lt.length += dataLength

	for {
		pos := bytes.IndexByte(lt.buf[lt.start:lt.length], '\n')
		if pos < 0 {
			break
		}

		if pos > 0 {
			err := lt.encode(wire.Log{Stream: lt.stream, Content: lt.buf[lt.start:pos]})
			if err != nil {
				return 0, err
			}
		}

		lt.start += pos + 1
		lt.length -= pos + 1

		if lt.start == len(lt.buf) {
			lt.start = 0
			lt.length = 0
		}
	}

	return dataLength, nil
}

func (lt *logTransmitter) Sync() error {
	return nil
}

func zapTransmitter(ctx context.Context, encode wire.EncoderFunc) context.Context {
	transmitter := newLogTransmitter(encode, wire.StreamErr)
	transmitCore := zapcore.NewCore(zapcore.NewJSONEncoder(logger.EncoderConfig), transmitter, zap.NewAtomicLevelAt(zap.DebugLevel))

	log := logger.Get(ctx)
	log = log.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
		return zapcore.NewTee(core, transmitCore)
	}))

	return logger.WithLogger(ctx, log)
}
