package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrEmergencyStop marks work an emergency stop refused or cancelled.
var ErrEmergencyStop = errors.New("emergency stop")

// statementCanceler cancels the statement running on one connection.
// *pgconn.PgConn's CancelRequest sends PostgreSQL's protocol cancel for its
// own backend: no grant is needed on any provider, and the session stays
// usable. pgx's default context handling closes the socket instead, which
// leaves a CIC waiting on a snapshot running on the server.
type statementCanceler interface {
	CancelRequest(ctx context.Context) error
}

// inflightRun is one Apply run or rollback while it may execute statements.
type inflightRun struct {
	label     string
	startedAt time.Time
	done      chan struct{}

	mu       sync.Mutex
	cause    error // set once by the stop
	stmts    map[statementCanceler]struct{}
	running  string // the statement last started, for the stop's report
	halted   bool   // the stop refused or failed one of its statements
	actionID int64
	ended    bool
}

// inflightRegistry tracks the executor's running work so an emergency stop
// can cancel it. Its zero value is ready to use.
type inflightRegistry struct {
	mu    sync.Mutex
	runs  map[*inflightRun]struct{}
	latch error         // the stop's cause until the executor is re-enabled
	wait  time.Duration // how long a stop waits for its runs; 0 = default
}

type inflightKey struct{}

// begin registers a run and returns a context that carries it. A latched
// stop refuses new runs.
func (r *inflightRegistry) begin(
	ctx context.Context, label string,
) (context.Context, *inflightRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latch != nil {
		return ctx, nil, &StoppedError{Cause: r.latch}
	}
	if r.runs == nil {
		r.runs = map[*inflightRun]struct{}{}
	}
	run := &inflightRun{label: label, startedAt: time.Now(), done: make(chan struct{}),
		stmts: map[statementCanceler]struct{}{}}
	r.runs[run] = struct{}{}
	return context.WithValue(ctx, inflightKey{}, run), run, nil
}

// end deregisters a run with the action_log id it recorded. Ending twice is
// a no-op that keeps the first id.
func (r *inflightRegistry) end(run *inflightRun, actionID int64) {
	if run == nil {
		return
	}
	r.mu.Lock()
	delete(r.runs, run)
	r.mu.Unlock()
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.ended {
		return
	}
	run.ended, run.actionID = true, actionID
	close(run.done)
}

// release clears the stop latch (the executor was re-enabled).
func (r *inflightRegistry) release() {
	r.mu.Lock()
	r.latch = nil
	r.mu.Unlock()
}

// stopAll latches the registry, so nothing new starts, and stops every
// registered run: each statement it is running gets a cancel request.
func (r *inflightRegistry) stopAll(ctx context.Context, cause error) map[*inflightRun]error {
	r.mu.Lock()
	r.latch = cause
	r.mu.Unlock()
	runs := r.snapshot()
	var mu sync.Mutex
	var wg sync.WaitGroup
	results := make(map[*inflightRun]error, len(runs))
	for _, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := run.stop(ctx, cause)
			mu.Lock()
			results[run] = err
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

func (r *inflightRegistry) snapshot() []*inflightRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	runs := make([]*inflightRun, 0, len(r.runs))
	for run := range r.runs {
		runs = append(runs, run)
	}
	return runs
}

func (r *inflightRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

// watchedStatements counts the statements registered runs are executing.
func (r *inflightRegistry) watchedStatements() int {
	n := 0
	for _, run := range r.snapshot() {
		run.mu.Lock()
		n += len(run.stmts)
		run.mu.Unlock()
	}
	return n
}

// stop marks the run stopped and cancels its running statements. The lock
// is held while the cancel requests are sent, so a statement's connection is
// not cleaned up or returned to the pool before its cancel was delivered.
func (run *inflightRun) stop(ctx context.Context, cause error) error {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.cause == nil {
		run.cause = cause
	}
	var errs []error
	for stmt := range run.stmts {
		if err := stmt.CancelRequest(ctx); err != nil {
			errs = append(errs, fmt.Errorf("cancel request: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (run *inflightRun) stopCause() error {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.cause
}

// describe names the run by the statement it last started, else its label.
func (run *inflightRun) describe() string {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.running != "" {
		return run.running
	}
	if run.label != "" {
		return run.label
	}
	return "action (no statement started yet)"
}

// halt marks a stopped run as halted by the stop: one of its statements
// failed after the stop's cancel. It returns the stop's cause, nil when the
// run was not stopped.
func (run *inflightRun) halt() error {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.cause != nil {
		run.halted = true
	}
	return run.cause
}

// cancelledID is the action_log id the run recorded when the stop halted
// it; a run that finished on its own during the stop has none.
func (run *inflightRun) cancelledID() int64 {
	run.mu.Lock()
	halted := run.halted
	run.mu.Unlock()
	if !halted {
		return 0
	}
	return run.result()
}

func (run *inflightRun) result() int64 {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.actionID
}

func runFromContext(ctx context.Context) *inflightRun {
	run, _ := ctx.Value(inflightKey{}).(*inflightRun)
	return run
}

// watchStatement registers conn as running sql for ctx's run until the
// returned unwatch. A stopped run refuses to start the statement. Work
// outside a run (probes, the ledger) is not tracked.
func watchStatement(
	ctx context.Context, conn statementCanceler, sql string,
) (func(), error) {
	run := runFromContext(ctx)
	if run == nil || conn == nil {
		return func() {}, nil
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.cause != nil {
		run.halted = true
		return nil, &StoppedError{Cause: run.cause}
	}
	run.stmts[conn] = struct{}{}
	run.running = sql
	return func() {
		run.mu.Lock()
		delete(run.stmts, conn)
		run.mu.Unlock()
	}, nil
}

// stoppedStatement reports err of sql as cancelled by the stop when ctx's
// run was stopped; other errors pass through.
func stoppedStatement(ctx context.Context, sql string, err error) error {
	if err == nil {
		return nil
	}
	var stopped *StoppedError
	if errors.As(err, &stopped) {
		if stopped.SQL == "" {
			stopped.SQL = sql
		}
		return err
	}
	run := runFromContext(ctx)
	if run == nil {
		return err
	}
	if cause := run.halt(); cause != nil {
		return &StoppedError{Cause: cause, SQL: sql, Err: err}
	}
	return err
}

// emergencyStopCause is the error a stop cancels work with; its text leads
// every failed row's reason.
func emergencyStopCause(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || strings.EqualFold(reason, ErrEmergencyStop.Error()) {
		return ErrEmergencyStop
	}
	return fmt.Errorf("%w (%s)", ErrEmergencyStop, reason)
}
