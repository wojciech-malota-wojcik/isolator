package main

import (
	"context"

	"github.com/spf13/pflag"
	"github.com/wojciech-malota-wojcik/isolator/executor"
	"github.com/wojciech-malota-wojcik/run"
)

func main() {
	var addr string
	pflag.StringVar(&addr, "addr", "unix:///tmp/executor.sock", "Address to listen on")
	pflag.Parse()

	run.Service("executor", nil, func(ctx context.Context) error {
		return executor.Run(ctx, addr)
	})
}
