package executor

import (
	"context"
	"os/exec"

	"github.com/outofforest/libexec"
	"github.com/pkg/errors"

	"github.com/outofforest/isolator/lib/docker"
	"github.com/outofforest/isolator/lib/libhttp"
	"github.com/outofforest/isolator/wire"
)

// NewInitFromDockerHandler creates new standard handler for InitFromDocker command.
func NewInitFromDockerHandler() HandlerFunc {
	// creating http client before pivoting/chrooting because client reads CA certificates from system pool
	httpClient := libhttp.NewSelfClient()

	return func(ctx context.Context, content interface{}, encode wire.EncoderFunc) error {
		m, ok := content.(wire.InitFromDocker)
		if !ok {
			return errors.Errorf("unexpected type %T", content)
		}
		return docker.Apply(ctx, httpClient, m.Image, m.Tag)
	}
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
