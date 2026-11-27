package executor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrCustodianBackoff is returned while a custodian proposal that keeps
// failing (execution or post-check) is backing off.
var ErrCustodianBackoff = errors.New("custodian proposal in failure backoff")

const (
	custodianMaxFailures   = 3
	custodianBackoffWindow = 6 * time.Hour
)

// custodianBackoffError is one backoff verdict: the failures in the window,
// the newest, and whether they reached custodianMaxFailures (exhausted: a
// full stop until they age out).
type custodianBackoffError struct {
	failures  int
	last      time.Time
	exhausted bool
	retryIn   time.Duration
}

func (e *custodianBackoffError) Error() string {
	if e.exhausted {
		return fmt.Sprintf("%s: %d failures in %s; escalate to an operator",
			ErrCustodianBackoff, e.failures, custodianBackoffWindow)
	}
	return fmt.Sprintf("%s: retry in %s", ErrCustodianBackoff, e.retryIn.Round(time.Second))
}

func (e *custodianBackoffError) Unwrap() error { return ErrCustodianBackoff }

// custodianBackoff throttles re-submission of a proposal whose recent
// attempts failed: exponential backoff after each failure, and a full stop
// (escalation to a human) once custodianMaxFailures failures accumulate in
// the window. State is the durable action_log, so it survives restarts.
func (e *Executor) custodianBackoff(ctx context.Context, sql string) error {
	if e.pool == nil || sql == "" {
		return nil
	}
	failures, last, err := e.readCustodianFailures(ctx, sql)
	if err != nil {
		return err
	}
	if failures == 0 || last == nil {
		return nil
	}
	if failures >= custodianMaxFailures {
		return &custodianBackoffError{failures: failures, last: *last, exhausted: true}
	}
	wait := time.Minute << failures
	if since := time.Since(*last); since < wait {
		return &custodianBackoffError{failures: failures, last: *last, retryIn: wait - since}
	}
	return nil
}

// readCustodianFailures counts sql's failed runs in the backoff window and
// returns the newest one's time.
func (e *Executor) readCustodianFailures(
	ctx context.Context, sql string,
) (int, *time.Time, error) {
	var failures int
	var last *time.Time
	err := e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT count(*), max(executed_at)
		FROM sage.action_log
		WHERE sql_executed = $1 AND outcome IN ('failed', 'rollback_failed')
		  AND executed_at > now() - make_interval(secs => $2)`,
		sql, custodianBackoffWindow.Seconds()).Scan(&failures, &last)
	if err != nil {
		return 0, nil, fmt.Errorf("read custodian failure history: %w", err)
	}
	return failures, last, nil
}
