package executor

import (
	"bytes"
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/outofforest/isolator/wire"
	"github.com/outofforest/logger"
)

func newLogTransmitter(encode wire.EncoderFunc) *logTransmitter {
	return &logTransmitter{
		encode: encode,
	}
}

type logTransmitter struct {
	encode wire.EncoderFunc

	mu     sync.Mutex
	buf    []byte
	start  int
	end    int
	length int
}

func (lt *logTransmitter) Write(data []byte) (int, error) {
	lt.mu.Lock()
	defer lt.mu.Unlock()

	bufLen := 1024

	dataLength := len(data)
	if dataLength == 0 {
		return 0, nil
	}

	if len(lt.buf[lt.end:]) < dataLength {
		if newBufLen := lt.length + dataLength; bufLen < newBufLen {
			bufLen = newBufLen
		}
		newBuf := make([]byte, bufLen)
		if lt.length > 0 {
			copy(newBuf, lt.buf[lt.start:lt.end])
		}
		lt.buf = newBuf
		lt.start = 0
		lt.end = lt.length
	}
	copy(lt.buf[lt.end:], data)
	lt.end += dataLength
	lt.length += dataLength

	for {
		if lt.length == 0 {
			break
		}
		pos := bytes.IndexByte(lt.buf[lt.start:lt.end], '\n')
		if pos < 0 {
			break
		}

		if pos > 0 {
			err := lt.encode(wire.Log{Time: time.Now().UTC(), Content: lt.buf[lt.start : lt.start+pos]})
			if err != nil {
				return 0, err
			}
		}

		lt.start += pos + 1
		lt.length -= pos + 1

		if lt.start == lt.end {
			lt.start = 0
			lt.end = 0
		}
	}

	return dataLength, nil
}

func (lt *logTransmitter) Sync() error {
	return nil
}

func withZAPTransmitter(ctx context.Context, encode wire.EncoderFunc) context.Context {
	transmitter := newLogTransmitter(encode)
	transmitCore := zapcore.NewCore(zapcore.NewJSONEncoder(logger.EncoderConfig), transmitter,
		zap.NewAtomicLevelAt(zap.DebugLevel))

	log := logger.Get(ctx)
	log = log.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
		return transmitCore
	}))

	return logger.WithLogger(ctx, log)
}
