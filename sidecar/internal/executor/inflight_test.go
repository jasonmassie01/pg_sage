package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// fakeStatement stands in for the connection of a running statement: its
// CancelRequest is what the stop sends to PostgreSQL.
type fakeStatement struct {
	cancelled chan struct{}
	once      sync.Once
	calls     atomic.Int32
	err       error
}

func newFakeStatement() *fakeStatement {
	return &fakeStatement{cancelled: make(chan struct{})}
}

func (f *fakeStatement) CancelRequest(context.Context) error {
	f.calls.Add(1)
	if f.err != nil {
		return f.err
	}
	f.once.Do(func() { close(f.cancelled) })
	return nil
}

// runStatement watches stmt as the running statement of ctx's run and blocks
// until the stop cancels it, as pgx blocks in Exec until PostgreSQL answers.
func runStatement(ctx context.Context, stmt *fakeStatement, sql string) error {
	unwatch, err := watchStatement(ctx, stmt, sql)
	if err != nil {
		return err
	}
	defer unwatch()
	select {
	case <-stmt.cancelled:
		return stoppedStatement(ctx, sql,
			errors.New("ERROR: canceling statement due to user request (SQLSTATE 57014)"))
	case <-time.After(5 * time.Second):
		return errors.New("statement was never cancelled")
	}
}

func allowExecute(context.Context) (ActionPolicyDecision, error) {
	return ActionPolicyDecision{Decision: PolicyDecisionExecute, DecisionID: 9}, nil
}

func inflightExecutor() *Executor {
	return New(nil, config.DefaultConfig(), time.Time{}, nopLog)
}

// blockingIntent runs one statement that only an emergency stop ends and
// reports actionID, like a CIC whose failed row is recorded after it returns.
func blockingIntent(stmt *fakeStatement, actionID int64, executed *atomic.Int32) ActionIntent {
	return ActionIntent{
		Authorize: allowExecute, SlotHeld: true,
		Execute: func(ctx context.Context, _ ActionPolicyDecision) (int64, error) {
			executed.Add(1)
			return actionID, runStatement(ctx, stmt, "VACUUM public.orders")
		},
	}
}

// waitWatched waits until n statements of e's runs are watched.
func waitWatched(t *testing.T, e *Executor, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.inflight.watchedStatements() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d of %d statements started", e.inflight.watchedStatements(), n)
}

func TestCancelInFlightWithNothingInFlightReturnsEmpty(t *testing.T) {
	e := inflightExecutor()
	ids, err := e.CancelInFlight("emergency stop")
	if err != nil {
		t.Fatalf("CancelInFlight with nothing running: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("cancelled ids = %v, want none", ids)
	}
}

func TestInFlightEndTwiceIsNoop(t *testing.T) {
	var r inflightRegistry
	_, run, err := r.begin(context.Background(), "ANALYZE public.orders")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if got := r.count(); got != 1 {
		t.Fatalf("registered runs = %d, want 1", got)
	}
	r.end(run, 11)
	r.end(run, 12)
	if got := r.count(); got != 0 {
		t.Fatalf("registered runs after end = %d, want 0", got)
	}
	select {
	case <-run.done:
	default:
		t.Fatal("ended run is not marked done")
	}
	if run.result() != 11 {
		t.Fatalf("action id = %d, want the first end's 11", run.result())
	}
}

// No in-flight run in the context: the statement runs untracked, as code
// outside Apply (probes, the ledger) always has.
func TestWatchStatementWithoutRunIsNoop(t *testing.T) {
	stmt := newFakeStatement()
	unwatch, err := watchStatement(context.Background(), stmt, "VACUUM t")
	if err != nil {
		t.Fatalf("watch without a run: %v", err)
	}
	unwatch()
	cause := errors.New("boom")
	if got := stoppedStatement(context.Background(), "VACUUM t", cause); got != cause {
		t.Fatalf("untracked error rewritten to %v", got)
	}
	if got := stoppedStatement(context.Background(), "VACUUM t", nil); got != nil {
		t.Fatalf("untracked success rewritten to %v", got)
	}
}

func TestCancelInFlightCancelsRunningStatementAndReportsActionID(t *testing.T) {
	e := inflightExecutor()
	stmt := newFakeStatement()
	var executed atomic.Int32
	errCh := make(chan error, 1)
	go func() {
		_, err := e.Apply(context.Background(), blockingIntent(stmt, 77, &executed))
		errCh <- err
	}()
	waitWatched(t, e, 1)
	started := time.Now()
	ids, err := e.CancelInFlight("requested by dba")
	if err != nil {
		t.Fatalf("CancelInFlight: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("stop took %s, want under 2s", elapsed)
	}
	if len(ids) != 1 || ids[0] != 77 {
		t.Fatalf("cancelled ids = %v, want [77]", ids)
	}
	if stmt.calls.Load() != 1 {
		t.Fatalf("cancel requests = %d, want 1", stmt.calls.Load())
	}
	applyErr := <-errCh
	if !errors.Is(applyErr, ErrEmergencyStop) {
		t.Fatalf("Apply error = %v, want ErrEmergencyStop", applyErr)
	}
	if !strings.HasPrefix(applyErr.Error(), "emergency stop") ||
		!strings.Contains(applyErr.Error(), "requested by dba") ||
		!strings.Contains(applyErr.Error(), "57014") {
		t.Fatalf("Apply error %q does not name the stop, its reason and the cancel", applyErr)
	}
	if e.inflight.count() != 0 {
		t.Fatalf("%d runs still registered after the stop", e.inflight.count())
	}
}

// State transitions: running -> stopped (latched: nothing new starts) ->
// resumed by re-enabling the executor.
func TestInFlightLatchRefusesNewRunsUntilExecutorReenabled(t *testing.T) {
	e := inflightExecutor()
	if _, err := e.CancelInFlight("emergency stop"); err != nil {
		t.Fatalf("CancelInFlight: %v", err)
	}
	var executed atomic.Int32
	_, err := e.Apply(context.Background(), blockingIntent(newFakeStatement(), 1, &executed))
	if !errors.Is(err, ErrEmergencyStop) || executed.Load() != 0 {
		t.Fatalf("Apply after stop: err=%v executed=%d, want refused before Execute",
			err, executed.Load())
	}
	if _, _, err := e.inflight.begin(context.Background(), "rollback"); !errors.Is(err,
		ErrEmergencyStop) {
		t.Fatalf("rollback begin after stop = %v, want ErrEmergencyStop", err)
	}
	e.SetExecutorEnabled(false)
	if _, _, err := e.inflight.begin(context.Background(), "x"); err == nil {
		t.Fatal("disabling the executor released the stop latch")
	}
	e.SetExecutorEnabled(true)
	ctx, run, err := e.inflight.begin(context.Background(), "after resume")
	if err != nil {
		t.Fatalf("begin after resume: %v", err)
	}
	defer e.inflight.end(run, 0)
	if _, err := watchStatement(ctx, newFakeStatement(), "ANALYZE t"); err != nil {
		t.Fatalf("statement after resume refused: %v", err)
	}
}

// A run stopped between statements never starts the next one.
func TestStoppedRunRefusesItsNextStatement(t *testing.T) {
	var r inflightRegistry
	ctx, run, err := r.begin(context.Background(), "replace index")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer r.end(run, 0)
	r.stopAll(context.Background(), emergencyStopCause(""))
	stmt := newFakeStatement()
	_, err = watchStatement(ctx, stmt, "DROP INDEX CONCURRENTLY public.idx_old")
	var stopped *StoppedError
	if !errors.As(err, &stopped) || !errors.Is(err, ErrEmergencyStop) {
		t.Fatalf("watch after stop = %v, want a StoppedError", err)
	}
	if stmt.calls.Load() != 0 || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("err=%q cancels=%d, want a refusal that sent nothing", err, stmt.calls.Load())
	}
}

func TestStoppedErrorNamesInvalidIndexRemnant(t *testing.T) {
	cases := []struct {
		sql, want string
	}{
		{"CREATE INDEX CONCURRENTLY idx_a ON public.orders (a)", "public.idx_a"},
		{"CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS \"Idx_B\" ON s.t (b)", `s."Idx_B"`},
		{"CREATE INDEX CONCURRENTLY ON public.orders (customer_id)",
			"public.orders_customer_id_idx"},
	}
	for _, tc := range cases {
		err := (&StoppedError{Cause: ErrEmergencyStop, SQL: tc.sql,
			Err: errors.New("canceling statement")}).Error()
		if !strings.Contains(err, "INVALID index "+tc.want) ||
			!strings.Contains(err, "DROP INDEX CONCURRENTLY IF EXISTS "+tc.want) {
			t.Errorf("%s: error %q does not name remnant %s and its cleanup", tc.sql, err, tc.want)
		}
	}
	for _, sql := range []string{"", "VACUUM public.orders", "CREATE INDEX idx_c ON t (c)",
		"DROP INDEX CONCURRENTLY public.idx_a", "CREATE INDEX CONCURRENTLY"} {
		err := (&StoppedError{Cause: ErrEmergencyStop, SQL: sql}).Error()
		if strings.Contains(err, "INVALID") || strings.Contains(err, "DROP INDEX") {
			t.Errorf("%q: error %q names a remnant it cannot leave", sql, err)
		}
	}
}

func TestCancelInFlightReportsFailedCancelPerRun(t *testing.T) {
	e := inflightExecutor()
	e.inflight.wait = 300 * time.Millisecond
	broken := newFakeStatement()
	broken.err = errors.New("dial tcp: connection refused")
	good := newFakeStatement()
	var executed atomic.Int32
	done := make(chan struct{}, 2)
	for _, run := range []struct {
		stmt *fakeStatement
		id   int64
	}{{broken, 5}, {good, 6}} {
		go func() {
			_, _ = e.Apply(context.Background(), blockingIntent(run.stmt, run.id, &executed))
			done <- struct{}{}
		}()
	}
	waitWatched(t, e, 2)
	ids, err := e.CancelInFlight("emergency stop")
	var failed *InFlightCancelError
	if !errors.As(err, &failed) || len(failed.Failures) != 1 {
		t.Fatalf("CancelInFlight error = %v, want one per-run failure", err)
	}
	f := failed.Failures[0]
	if !strings.Contains(f.Err.Error(), "connection refused") ||
		!strings.Contains(f.Err.Error(), "still running") || f.Label != "VACUUM public.orders" {
		t.Fatalf("failure = %+v, want the cancel error and the run left running", f)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("error text %q hides the failed cancel", err)
	}
	if len(ids) != 1 || ids[0] != 6 {
		t.Fatalf("cancelled ids = %v, want only the confirmed run [6]", ids)
	}
	<-done // the good run
	select {
	case <-done:
		t.Fatal("the run whose cancel failed ended without a cancel")
	default:
	}
	broken.err = nil
	_ = broken.CancelRequest(context.Background()) // let the goroutine exit
	<-done
}

// pausedIntent blocks in Execute, between statements, until finish closes,
// then reports a successful row id.
func pausedIntent(inExecute, finish chan struct{}, id int64) ActionIntent {
	return ActionIntent{
		Authorize: allowExecute, SlotHeld: true,
		Execute: func(context.Context, ActionPolicyDecision) (int64, error) {
			close(inExecute)
			<-finish
			return id, nil
		},
	}
}

// Boundary: a run the stop finds between statements that then finishes on
// its own recorded a successful row; listing it as cancelled would tell the
// operator a change that happened did not.
func TestCancelInFlightDoesNotListRunThatFinishedOnItsOwn(t *testing.T) {
	e := inflightExecutor()
	inExecute, finish := make(chan struct{}), make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		_, err := e.Apply(context.Background(), pausedIntent(inExecute, finish, 42))
		errCh <- err
	}()
	<-inExecute
	time.AfterFunc(100*time.Millisecond, func() { close(finish) })
	ids, err := e.CancelInFlight("emergency stop")
	if err != nil || len(ids) != 0 {
		t.Fatalf("CancelInFlight = (%v, %v), want no cancelled ids and no error", ids, err)
	}
	if applyErr := <-errCh; applyErr != nil {
		t.Fatalf("Apply = %v, want the run's own success", applyErr)
	}
}

// Error propagation: a run still busy before its first statement is named
// in the failure even though it has no statement or label yet.
func TestCancelInFlightNamesRunWithoutStatement(t *testing.T) {
	e := inflightExecutor()
	e.inflight.wait = 100 * time.Millisecond
	inExecute, finish := make(chan struct{}), make(chan struct{})
	defer close(finish)
	go func() { _, _ = e.Apply(context.Background(), pausedIntent(inExecute, finish, 1)) }()
	<-inExecute
	ids, err := e.CancelInFlight("emergency stop")
	var failed *InFlightCancelError
	if !errors.As(err, &failed) || len(failed.Failures) != 1 || len(ids) != 0 {
		t.Fatalf("CancelInFlight = (%v, %v), want one failure and no ids", ids, err)
	}
	if got := failed.Failures[0].Label; got != "action (no statement started yet)" {
		t.Fatalf("failure label = %q, want the placeholder for an unnamed run", got)
	}
}

// droppedCancel is a running statement whose first cancel request reaches
// the backend between two statements, where PostgreSQL drops it; only a
// repeated cancel ends it.
type droppedCancel struct {
	calls     atomic.Int32
	cancelled chan struct{}
	once      sync.Once
}

func (d *droppedCancel) CancelRequest(context.Context) error {
	if d.calls.Add(1) > 1 {
		d.once.Do(func() { close(d.cancelled) })
	}
	return nil
}

// Boundary: a cancel PostgreSQL dropped is sent again, so the statement
// still ends within the stop's wait and its row is listed as cancelled.
func TestCancelInFlightResendsDroppedCancel(t *testing.T) {
	e := inflightExecutor()
	stmt := &droppedCancel{cancelled: make(chan struct{})}
	errCh := make(chan error, 1)
	go func() {
		_, err := e.Apply(context.Background(), ActionIntent{
			Authorize: allowExecute, SlotHeld: true,
			Execute: func(ctx context.Context, _ ActionPolicyDecision) (int64, error) {
				unwatch, err := watchStatement(ctx, stmt, "VACUUM public.orders")
				if err != nil {
					return 0, err
				}
				defer unwatch()
				<-stmt.cancelled
				return 31, stoppedStatement(ctx, "VACUUM public.orders",
					errors.New("canceling statement due to user request (SQLSTATE 57014)"))
			},
		})
		errCh <- err
	}()
	waitWatched(t, e, 1)
	ids, err := e.CancelInFlight("emergency stop")
	if err != nil || len(ids) != 1 || ids[0] != 31 {
		t.Fatalf("CancelInFlight = (%v, %v), want [31] after a repeated cancel", ids, err)
	}
	if n := stmt.calls.Load(); n < 2 {
		t.Fatalf("cancel requests = %d, want the dropped one resent", n)
	}
	if applyErr := <-errCh; !errors.Is(applyErr, ErrEmergencyStop) {
		t.Fatalf("Apply = %v, want ErrEmergencyStop", applyErr)
	}
}
