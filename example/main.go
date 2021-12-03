package main

import (
	"context"
	"fmt"
	"os"

	"github.com/wojciech-malota-wojcik/isolator"
	"github.com/wojciech-malota-wojcik/isolator/client/wire"
	"github.com/wojciech-malota-wojcik/logger"
)

func main() {
	// Starting isolator. If passed ctx is canceled, isolator.Start breaks and returns error.
	// Isolator creates `root` directory under one passed to `isolator.Start`. The `root` directory is mounted as `/`.
	// inside container.
	// It is assumed that `root` contains `bin/sh` shell and all the required libraries. Without them it will fail.
	isolator, terminateIsolator, err := isolator.Start(logger.WithLogger(context.Background(), logger.New()), "/tmp/example")
	if err != nil {
		panic(err)
	}
	defer func() {
		// Clean up on exit
		if err := terminateIsolator(); err != nil {
			panic(err)
		}
	}()

	// Request to execute command in isolation
	if err := isolator.Send(wire.Execute{Command: `echo "Hello world!"`}); err != nil {
		panic(err)
	}

	// Communication channel loop
	for {
		msg, err := isolator.Receive()
		if err != nil {
			panic(err)
		}
		switch m := msg.(type) {
		// wire.Log contains message printed by executed command to stdout or stderr
		case wire.Log:
			stream, err := toStream(m.Stream)
			if err != nil {
				panic(err)
			}
			if _, err := stream.Write([]byte(m.Text)); err != nil {
				panic(err)
			}
		// wire.Completed means command finished
		case wire.Completed:
			if m.ExitCode != 0 || m.Error != "" {
				panic(fmt.Errorf("command failed: %s, exit code: %d", m.Error, m.ExitCode))
			}
			return
		default:
			panic("unexpected message received")
		}
	}
}

func toStream(stream wire.Stream) (*os.File, error) {
	var f *os.File
	switch stream {
	case wire.StreamOut:
		f = os.Stdout
	case wire.StreamErr:
		f = os.Stderr
	default:
		return nil, fmt.Errorf("unknown stream: %d", stream)
	}
	return f, nil
}
