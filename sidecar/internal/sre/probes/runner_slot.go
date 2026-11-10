package probes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrConcurrencyLimit is a read that found no free probe slot within
// QueueWait (reason concurrency_limit, PhaseQueue, as for a probe).
var ErrConcurrencyLimit = errors.New("probe concurrency limit")

// errRunnerNotConfigured is WithSlot on a nil runner.
var errRunnerNotConfigured = errors.New("probe runner not configured")

// WithSlot runs fn while it holds this database's probe slot and a
// sidecar slot, the same slots Run takes: a read the runner does not make
// itself (the investigator's plan-only EXPLAIN) never runs beside a probe
// on the same database. When no slot frees up within QueueWait it returns
// an ErrConcurrencyLimit error without calling fn; otherwise fn's error.
func (r *Runner) WithSlot(ctx context.Context, fn func(context.Context) error) error {
	if r == nil || r.local == nil || r.global == nil {
		return errRunnerNotConfigured
	}
	release, err := r.acquire(ctx)
	if err != nil {
		return fmt.Errorf("%w: waiting for a probe slot: %w", ErrConcurrencyLimit, err)
	}
	defer release()
	return fn(ctx)
}

// Pool is the monitored database's pool (nil on a nil runner).
func (r *Runner) Pool() *pgxpool.Pool {
	if r == nil {
		return nil
	}
	return r.pool
}

// AcquireWait bounds one pool acquire of a read under WithSlot, separate
// from the statement's budget.
func (r *Runner) AcquireWait() time.Duration {
	if r == nil || r.acquireWait <= 0 {
		return acquireWait
	}
	return r.acquireWait
}
