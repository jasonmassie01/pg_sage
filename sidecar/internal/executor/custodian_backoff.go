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

// custodianBackoff throttles re-submission of a proposal whose recent
// attempts failed: exponential backoff after each failure, and a full stop
// (escalation to a human) once custodianMaxFailures failures accumulate in
// the window. State is the durable action_log, so it survives restarts.
func (e *Executor) custodianBackoff(ctx context.Context, sql string) error {
	if e.pool == nil || sql == "" {
		return nil
	}
	var failures int
	var last *time.Time
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT count(*), max(executed_at)
		FROM sage.action_log
		WHERE sql_executed = $1 AND outcome IN ('failed', 'rollback_failed')
		  AND executed_at > now() - make_interval(secs => $2)`,
		sql, custodianBackoffWindow.Seconds()).Scan(&failures, &last)
	if err != nil {
		return fmt.Errorf("read custodian failure history: %w", err)
	}
	if failures == 0 || last == nil {
		return nil
	}
	if failures >= custodianMaxFailures {
		return fmt.Errorf("%w: %d failures in %s; escalate to an operator",
			ErrCustodianBackoff, failures, custodianBackoffWindow)
	}
	wait := time.Minute << failures
	if since := time.Since(*last); since < wait {
		return fmt.Errorf("%w: retry in %s", ErrCustodianBackoff, (wait - since).Round(time.Second))
	}
	return nil
}
