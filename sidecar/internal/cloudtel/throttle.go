package cloudtel

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// A throttled read (HTTP 429) is retried a few times inside the poll: the
// waits grow from throttleBaseDelay (or follow Retry-After, capped), and a
// wait that would pass the poll's deadline is not slept. A read still
// throttled after that fails the poll with ErrThrottled, and the runtime's
// own backoff takes over.
const (
	throttleRetries   = 2
	throttleBaseDelay = 500 * time.Millisecond
	maxRetryAfter     = 5 * time.Second
)

// sleepFunc waits d or until ctx ends (the error is ctx's).
type sleepFunc func(ctx context.Context, d time.Duration) error

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryThrottled runs call until it is not throttled or the retries are
// spent. call returns the body, the provider's Retry-After hint (0: none)
// and the error.
func retryThrottled(ctx context.Context, sleep sleepFunc,
	call func() ([]byte, time.Duration, error)) ([]byte, error) {
	if sleep == nil {
		sleep = sleepContext
	}
	for attempt := 0; ; attempt++ {
		raw, hint, err := call()
		if err == nil || !errors.Is(err, ErrThrottled) || attempt == throttleRetries {
			return raw, err
		}
		wait := throttleBaseDelay << attempt
		if hint > 0 {
			wait = min(hint, maxRetryAfter)
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
			return nil, err
		}
		if serr := sleep(ctx, wait); serr != nil {
			return nil, serr
		}
	}
}

// retryAfter parses a Retry-After header given in seconds (0: absent or
// an HTTP date, which providers do not send for throttling).
func retryAfter(header string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}
