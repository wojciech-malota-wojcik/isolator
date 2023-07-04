package retry

import (
	"context"
	"time"

	"github.com/pkg/errors"

	"go.uber.org/zap"

	"github.com/outofforest/logger"
)

// DelayFn is the type of function that can be called repeatedly to produce
// delays between attempts. A single value of DelayFn represents a single
// sequence of delays.
//
// Each call returns the delay before the next attempt, and a boolean value to
// indicate whether the next attempt is desired. If ok is false, the caller
// should stop trying and ignore the returned delay value. The caller is not
// expected to call the function again after receiving false.
//
// In other words, a DelayFn returns a finite or infinite sequence of delays
// over multiple calls. A false value of the second return value means the end
// of the sequence.
//
// -- Hey delay function, should I make an attempt?
// -- Yes, in two seconds (2*time.Second, true).
// -- Hey delay function, should I make an attempt?
// -- Yes, in four seconds (4*time.Second, true).
// -- Hey delay function, should I make an attempt?
// -- No (0, false).
//
// The delay function must return true as ok from the first call.
//
// Note that the first delay returned by the function is used before the very
// first attempt. For this reason, in most cases, the first call should return
// (0, true).
type DelayFn func() (delay time.Duration, ok bool)

// Config defines retry intervals.
//
// An implementation of Config is normally stateless.
type Config interface {
	// Delays returns a DelayFn representing the sequence of delays to use between attempts.
	// Each call to Delays returns a DelayFn representing an independent sequence.
	Delays() DelayFn
}

// FixedConfig defines fixed retry intervals
type FixedConfig struct {
	// TryAfter is the delay before the first attempt
	TryAfter time.Duration

	// RetryAfter is the delay before each subsequent attempt
	RetryAfter time.Duration

	// MaxAttempts is the maximum number of attempts taken; 0 = unlimited
	MaxAttempts int
}

// Delays implements interface Config
func (c FixedConfig) Delays() DelayFn {
	attempts := 0
	return func() (time.Duration, bool) {
		attempts++
		switch {
		case attempts == 1:
			return c.TryAfter, true
		case c.MaxAttempts != 0 && attempts > c.MaxAttempts:
			return 0, false
		default:
			return c.RetryAfter, true
		}
	}
}

// Immediately means retry without any delays
var Immediately = FixedConfig{}

// RetriableError means the operation that caused the error should be retried.
type RetriableError struct {
	err error
}

func (r RetriableError) Error() string {
	return r.err.Error()
}

// Unwrap returns the next error in the error chain.
func (r RetriableError) Unwrap() error {
	return r.err
}

// Retriable wraps an error to tell Do that it should keep trying.
// Returns nil if err is nil.
func Retriable(err error) error {
	if err == nil {
		return nil
	}
	return RetriableError{err: err}
}

// Do executes the given function, retrying if necessary.
//
// The given Config is used to calculate the delays before each attempt.
//
// Wrap an error with Retriable to indicate that Do should try again.
//
// If the function returns success, or an error that isn't wrapped,
// Do returns that value immediately without trying more.
//
// A RetriableError error will be logged unless its message is exactly the same as
// the previous one.
func Do(ctx context.Context, c Config, f func() error) error {
	delays := c.Delays()
	logger := logger.Get(ctx)
	var lastMessage string
	var r RetriableError
	for {
		delay, ok := delays()
		if !ok {
			if r.err == nil {
				panic("ok is false on first attempt")
			}
			return r.err
		}

		if err := Sleep(ctx, delay); err != nil {
			return err
		}

		if err := f(); !errors.As(err, &r) {
			return err
		}
		if errors.Is(r.err, ctx.Err()) {
			return r.err // f wants to retry but the context is closing
		}

		newMessage := r.err.Error()
		if lastMessage != newMessage {
			logger.Debug("Attempt failed, will retry", zap.Error(r.err))
			lastMessage = newMessage
		}
	}
}
