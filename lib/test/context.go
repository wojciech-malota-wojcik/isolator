package test

import (
	"context"
	"testing"

	"github.com/outofforest/logger"
)

// Context returns new context with logger.
func Context(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	return logger.WithLogger(ctx, logger.New(logger.DefaultConfig))
}
