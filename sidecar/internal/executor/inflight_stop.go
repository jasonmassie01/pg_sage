package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// defaultInFlightWait bounds how long a stop waits for its cancelled runs to
// record their outcome, so the stop itself answers within two seconds.
const defaultInFlightWait = 1500 * time.Millisecond

// recancelInterval paces the stop's repeated cancel requests. PostgreSQL
// drops a cancel that reaches a backend between two statements (after the
// session's SET, before the DDL), so a statement still watched afterwards
// gets the cancel again.
const recancelInterval = 200 * time.Millisecond

// StoppedError is a statement an emergency stop refused to start or
// cancelled while it ran. pg_sage runs no DDL during a stop, so a cancelled
// CREATE INDEX CONCURRENTLY names the INVALID index it may leave and the
// statement that removes it.
type StoppedError struct {
	Cause error  // wraps ErrEmergencyStop and gives the stop's reason
	SQL   string // the statement, when one was running
	Err   error  // the statement's own error; nil when it never started
}

func (e *StoppedError) Error() string {
	if e.Err == nil {
		return e.Cause.Error() + ": statement not started"
	}
	msg := e.Cause.Error() + ": cancelled the running statement"
	if remnant := cicRemnant(e.SQL); remnant != "" {
		msg += "; CREATE INDEX CONCURRENTLY may have left INVALID index " + remnant +
			"; remove it with: DROP INDEX CONCURRENTLY IF EXISTS " + remnant
	}
	return msg + " (" + e.Err.Error() + ")"
}

func (e *StoppedError) Unwrap() []error {
	if e.Err == nil {
		return []error{e.Cause}
	}
	return []error{e.Cause, e.Err}
}

// cicRemnant is the qualified name of the index a CREATE INDEX CONCURRENTLY
// statement builds, or "" for any other statement.
func cicRemnant(sql string) string {
	upper := strings.ToUpper(normalizeSQLText(sql))
	if !strings.HasPrefix(upper, "CREATE ") || !strings.Contains(upper, " CONCURRENTLY ") {
		return ""
	}
	schemaName, name, ok := createIndexName(sql)
	if !ok {
		return ""
	}
	return qualifiedName(schemaName, name)
}

// InFlightFailure is a run the stop could not confirm cancelled.
type InFlightFailure struct {
	Label     string
	StartedAt time.Time
	Err       error
}

// InFlightCancelError lists, per run, the cancel requests that failed and
// the runs still running when the stop stopped waiting.
type InFlightCancelError struct {
	Failures []InFlightFailure
}

func (e *InFlightCancelError) Error() string {
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		parts = append(parts, fmt.Sprintf("%q (started %s): %v",
			f.Label, f.StartedAt.Format(time.RFC3339), f.Err))
	}
	return "emergency stop could not confirm every running action cancelled: " +
		strings.Join(parts, "; ")
}

func (e *InFlightCancelError) Unwrap() []error {
	errs := make([]error, 0, len(e.Failures))
	for _, f := range e.Failures {
		errs = append(errs, f.Err)
	}
	return errs
}

// CancelInFlight is the emergency stop's halt of running work: it latches
// the executor, so no run starts until SetExecutorEnabled(true), cancels the
// statement every Apply run and rollback is executing, and waits briefly
// for them to record their outcome. It returns the action_log ids the
// cancelled runs recorded (rows end failed with reason "emergency stop"; a
// run that finished on its own while the stop waited is not listed) and
// an *InFlightCancelError naming each run whose cancel failed or which was
// still running. Operator-approved runs are cancelled too: a stop halts
// everything.
func (e *Executor) CancelInFlight(reason string) ([]int64, error) {
	wait := e.inflight.wait
	if wait <= 0 {
		wait = defaultInFlightWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	cause := emergencyStopCause(reason)
	stopped := e.inflight.stopAll(ctx, cause)
	recancelUntilEnded(ctx, cause, stopped)
	ids := []int64{}
	var failures []InFlightFailure
	for run, cancelErr := range stopped {
		if runEnded(ctx, run) {
			if id := run.cancelledID(); id > 0 {
				ids = append(ids, id)
			}
		} else {
			cancelErr = errors.Join(cancelErr,
				fmt.Errorf("still running %s after the cancel request", wait))
		}
		if cancelErr != nil {
			failures = append(failures, InFlightFailure{
				Label: run.describe(), StartedAt: run.startedAt, Err: cancelErr})
		}
	}
	if len(failures) > 0 {
		return ids, &InFlightCancelError{Failures: failures}
	}
	return ids, nil
}

// recancelUntilEnded resends the cancel requests of the runs that have not
// ended until they all have or ctx is done. Only statements a run is still
// executing are watched, and a stopped run starts no new one, so a repeated
// cancel never reaches unrelated work. The first attempt's errors are the
// ones reported.
func recancelUntilEnded(ctx context.Context, cause error, runs map[*inflightRun]error) {
	ticker := time.NewTicker(recancelInterval)
	defer ticker.Stop()
	for {
		var pending []*inflightRun
		for run := range runs {
			select {
			case <-run.done:
			default:
				pending = append(pending, run)
			}
		}
		if len(pending) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, run := range pending {
			_ = run.stop(ctx, cause)
		}
	}
}

// runEnded waits for run to end until ctx is done; a run that ended by
// then counts even when the deadline passed while waiting on another.
func runEnded(ctx context.Context, run *inflightRun) bool {
	select {
	case <-run.done:
		return true
	case <-ctx.Done():
	}
	select {
	case <-run.done:
		return true
	default:
		return false
	}
}

// trackRun registers a rollback as in-flight work for the emergency stop.
func (e *Executor) trackRun(
	ctx context.Context, label string,
) (context.Context, func(), error) {
	tracked, run, err := e.inflight.begin(ctx, label)
	if err != nil {
		return ctx, func() {}, err
	}
	return tracked, func() { e.inflight.end(run, 0) }, nil
}
