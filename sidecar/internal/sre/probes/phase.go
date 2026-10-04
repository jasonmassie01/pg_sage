package probes

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// Phase names where a probe run spent its time when it failed. Each phase
// has its own budget, so waiting for a slot or a connection never spends
// the statement's (dogfood lifeos, 2026-10-04: xid_runway, 0.1 ms on the
// server, failed with deadline_exceeded behind a busy 3-connection pool).
type Phase string

// Probe phases.
const (
	// PhaseQueue is the wait for a concurrency slot (queueWait).
	PhaseQueue Phase = "queue_wait"
	// PhasePoolAcquire is the wait for a pool connection (acquireWait).
	PhasePoolAcquire Phase = "pool_acquire"
	// PhaseExecution is the transaction on the server: settings, role
	// and extension checks, the statement and reading its rows (the
	// statement timeout plus clientMargin).
	PhaseExecution Phase = "server_execution"
	// PhaseLockWait is an execution that failed waiting for a lock (the
	// spec's lock_timeout).
	PhaseLockWait Phase = "lock_wait"
)

// Timing is where one probe run spent its time, summed over attempts.
type Timing struct {
	Queue     time.Duration
	Acquire   time.Duration
	Execution time.Duration
	Attempts  int
}

// Retry policy: a server-side timeout of a read-only catalog probe is
// retried once after a short jittered pause, and only when the caller's
// deadline leaves room for a whole attempt.
const (
	probeAttempts   = 2
	retryBackoffMin = 25 * time.Millisecond
	retryBackoffMax = 100 * time.Millisecond
	// clientMargin is how long past the statement timeout the client
	// waits for the server's answer.
	clientMargin = time.Second
)

// retryable reports a failure worth one more attempt: the server ran out
// of time (statement or lock timeout) or the answer did not arrive in
// time. A pool or queue wait already had its own budget.
func retryable(reason string, phase Phase) bool {
	switch reason {
	case "statement_timeout", "lock_timeout":
		return phase == PhaseExecution || phase == PhaseLockWait
	case "deadline_exceeded":
		return phase == PhaseExecution
	}
	return false
}

// retryAllowed reports whether another attempt may start after attempts
// were made: within the attempt budget, for a live caller whose deadline
// (if any) leaves room for a whole attempt.
func retryAllowed(ctx context.Context, spec Spec, attempts int) bool {
	if attempts >= probeAttempts || ctx.Err() != nil {
		return false
	}
	if dl, ok := ctx.Deadline(); ok &&
		time.Until(dl) < spec.StatementTimeout+clientMargin+retryBackoffMax {
		return false
	}
	return true
}

// retryBackoff is a jittered pause in [retryBackoffMin, retryBackoffMax].
func retryBackoff() time.Duration {
	span := int64(retryBackoffMax - retryBackoffMin)
	return retryBackoffMin + time.Duration(rand.Int64N(span+1))
}

// phaseFor is the phase a failure in stage is reported in: a lock
// timeout during execution is a lock wait.
func phaseFor(reason string, stage Phase) Phase {
	if reason == "lock_timeout" {
		return PhaseLockWait
	}
	return stage
}

// failedAt marks res as failed in stage, keeping its timing.
func failedAt(res Result, stage Phase, st Status, reason string, err error) Result {
	res = failed(res, st, reason, err)
	res.Phase = phaseFor(reason, stage)
	return res
}

// describe is the phase and timing suffix of an unavailable probe's error.
func (t Timing) describe(p Phase) string {
	return fmt.Sprintf(" in %s (queue %d ms, pool acquire %d ms, server execution %d ms, "+
		"%d attempt(s))", p, t.Queue.Milliseconds(), t.Acquire.Milliseconds(),
		t.Execution.Milliseconds(), t.Attempts)
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
