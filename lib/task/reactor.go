package task

import (
	"context"
	"fmt"

	"github.com/pkg/errors"

	"github.com/outofforest/logger"
	"github.com/outofforest/parallel"
)

// Func is the function containing a logic of the task.
type Func func(ctx context.Context) error

// Task is the task to execute in the reactor.
type Task struct {
	ID string
	Do Func
}

// SourceFunc is a function producing tasks.
type SourceFunc func(ctx context.Context, taskCh chan<- Task, doneCh <-chan Task) error

// Run processes tasks.
func Run(ctx context.Context, doneCh chan Task, sourceFunc SourceFunc) error {
	const workers = 5

	log := logger.Get(ctx)
	log.Info("Task reactor started")
	defer log.Info("Task reactor terminated")

	return parallel.Run(ctx, func(ctx context.Context, spawn parallel.SpawnFn) error {
		taskCh := make(chan Task, 1000)

		spawn("source", parallel.Continue, func(ctx context.Context) error {
			defer close(taskCh)
			return sourceFunc(ctx, taskCh, doneCh)
		})

		for i := range workers {
			spawn(fmt.Sprintf("worker-%d", i), parallel.Continue, func(ctx context.Context) error {
				for {
					select {
					case <-ctx.Done():
						return errors.WithStack(ctx.Err())
					case t, ok := <-taskCh:
						if !ok {
							return errors.WithStack(ctx.Err())
						}

						if err := t.Do(ctx); err != nil {
							return err
						}

						if doneCh == nil {
							continue
						}

						select {
						case <-ctx.Done():
							return errors.WithStack(ctx.Err())
						case doneCh <- t:
						}
					}
				}
			})
		}

		return nil
	})
}
