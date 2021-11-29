package main

import (
	"context"

	"github.com/spf13/pflag"
	"github.com/wojciech-malota-wojcik/isolator"
	"github.com/wojciech-malota-wojcik/run"
)

func main() {
	var config isolator.Config
	pflag.StringVar(&config.RootDir, "root-dir", ".", "Directory which will become a root of filesystem")
	pflag.StringVar(&config.ExecutorPath, "executor", "/tmp/executor", "File path where executor binary will be saved")
	pflag.StringVar(&config.Address, "addr", "unix:///tmp/isolator.sock", "Address executor will listen on")
	pflag.Parse()

	run.Service("isolator", nil, func(ctx context.Context) error {
		return isolator.Run(ctx, config)
	})
}
