package backtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Weekly runs a database's back-test once a week on the leader, so
// lab-01c's auto keeps a fresh verdict (it falls back to shadow when the
// latest passing row is older than 35 days). A database under the host
// withhold (managed-cloud host telemetry says to wait) is skipped: the
// replay reads a lot of history. A run that fails (not enough history
// yet, cut short at verify.backtest_max_runtime_minutes, a read error) is
// not retried before RetryAfter, so a database whose run cannot finish is
// not replayed for up to the whole runtime every Check. Run and Latest are
// the store's RunAndSave and Latest in the runtime. Tick is not safe for
// concurrent use: Loop calls it from one goroutine.
type Weekly struct {
	Config     Config
	Period     time.Duration // between runs (default 7 days)
	Check      time.Duration // how often Due is checked (default 1 hour)
	RetryAfter time.Duration // after a failed run (default 1 day)
	Leader     func() bool
	Withhold   func(context.Context) []string
	Latest     func(context.Context) (Result, bool, error)
	Run        func(context.Context, Config) (Result, error)
	Log        func(component, format string, args ...any)
	Now        func() time.Time

	failedAt time.Time // the last failed run; zero after a stored one
}

// Default cadence of the weekly run.
const (
	DefaultPeriod     = 7 * day
	DefaultCheck      = time.Hour
	DefaultRetryAfter = day
)

// Tick runs the back-test when it is due: on the leader, outside the host
// withhold, with no row from the last Period. It reports what it did.
func (w *Weekly) Tick(ctx context.Context) (string, error) {
	switch {
	case w.Run == nil || w.Latest == nil:
		return "", errors.New("the weekly verifier back-test needs a runner and a reader")
	case w.Leader != nil && !w.Leader():
		return "skipped: not the leader", nil
	}
	if w.Withhold != nil {
		if reasons := w.Withhold(ctx); len(reasons) > 0 {
			return "skipped: host withhold: " + strings.Join(reasons, "; "), nil
		}
	}
	now := w.now()
	if retry := w.failedAt.Add(w.retryAfter()); !w.failedAt.IsZero() && now.Before(retry) {
		return "skipped: the last run failed; retrying after " +
			retry.UTC().Format(time.RFC3339), nil
	}
	last, ok, err := w.Latest(ctx)
	if err != nil {
		return "", fmt.Errorf("weekly verifier back-test of %q: %w", w.Config.Database, err)
	}
	if ok && now.Sub(last.RanAt) < w.period() {
		return "skipped: last ran " + last.RanAt.UTC().Format(time.RFC3339), nil
	}
	cfg := w.Config
	cfg.Now = w.Now
	r, err := w.Run(ctx, cfg)
	if err != nil {
		w.failedAt = now
		return "", fmt.Errorf("weekly verifier back-test of %q (next try in %s): %w",
			w.Config.Database, w.retryAfter(), err)
	}
	w.failedAt = time.Time{}
	ok, reason := r.Gate()
	if ok {
		return "stored: gate passed", nil
	}
	return "stored: gate refused: " + reason, nil
}

func (w *Weekly) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Weekly) period() time.Duration {
	if w.Period > 0 {
		return w.Period
	}
	return DefaultPeriod
}

func (w *Weekly) retryAfter() time.Duration {
	if w.RetryAfter > 0 {
		return w.RetryAfter
	}
	return DefaultRetryAfter
}

// Loop ticks every Check until ctx ends, logging each run and failure.
func (w *Weekly) Loop(ctx context.Context) {
	every := w.Check
	if every <= 0 {
		every = DefaultCheck
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		did, err := w.Tick(ctx)
		w.log(did, err)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Weekly) log(did string, err error) {
	switch {
	case w.Log == nil:
	case err != nil:
		w.Log("verify", "verifier back-test: %v", err)
	case strings.HasPrefix(did, "stored"):
		w.Log("verify", "verifier back-test of %q %s", w.Config.Database, did)
	}
}
