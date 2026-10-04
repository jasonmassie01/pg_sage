package catalogread

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Phase is where a bounded read spent its time when it timed out.
type Phase string

// Read phases.
const (
	// PhaseBegin is getting a pool connection and opening the bounded
	// transaction (BEGIN and its settings).
	PhaseBegin Phase = "pool_acquire"
	// PhaseExecution is the server running the statement and sending its
	// rows.
	PhaseExecution Phase = "server_execution"
	// PhaseLockWait is a statement that timed out waiting for a lock.
	PhaseLockWait Phase = "lock_wait"
)

// PhaseError is a timed-out read annotated with the phase that spent the
// time. It wraps the driver's error: errors.As still finds the
// *pgconn.PgError.
type PhaseError struct {
	Phase     Phase
	Begin     time.Duration
	Execution time.Duration
	Err       error
}

func (e *PhaseError) Error() string {
	return fmt.Sprintf("%v (phase %s: pool acquire %d ms, server execution %d ms)", e.Err,
		e.Phase, e.Begin.Milliseconds(), e.Execution.Milliseconds())
}

func (e *PhaseError) Unwrap() error { return e.Err }

// SQLSTATEs of the server-side bounds.
const (
	sqlStateStatementTimeout = "57014"
	sqlStateLockTimeout      = "55P03"
)

// phased annotates a timeout with its phase; any other error (no rows, a
// missing relation, a permission) is returned as it is.
func phased(err error, phase Phase, begin, exec time.Duration) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == sqlStateLockTimeout:
		phase = PhaseLockWait
	case errors.As(err, &pgErr) && pgErr.Code == sqlStateStatementTimeout:
	case errors.Is(err, context.DeadlineExceeded):
	default:
		return err
	}
	return &PhaseError{Phase: phase, Begin: begin, Execution: exec, Err: err}
}

// Retryable reports a read worth one more attempt: the server ran out of
// time (statement or lock timeout), or the answer did not arrive before
// the client's deadline. Waiting for a connection already had its budget.
func Retryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == sqlStateStatementTimeout || pgErr.Code == sqlStateLockTimeout
	}
	var pe *PhaseError
	return errors.As(err, &pe) && pe.Phase == PhaseExecution &&
		errors.Is(err, context.DeadlineExceeded)
}
