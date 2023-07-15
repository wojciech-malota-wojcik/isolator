package tcontext

import (
	"context"
	"time"
)

// Reopen returns a context that inherits all the values stored in the given
// parent context, but not tied to the parent's lifespan. The returned context
// has no deadline.
//
// Reopen can even be used on an already closed context, hence the name.
func Reopen(ctx context.Context) context.Context {
	return reopened{Context: ctx}
}

type reopened struct {
	//nolint:containedctx // it's fine
	context.Context
}

func (reopened) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (reopened) Done() <-chan struct{} {
	return nil
}

func (reopened) Err() error {
	return nil
}
