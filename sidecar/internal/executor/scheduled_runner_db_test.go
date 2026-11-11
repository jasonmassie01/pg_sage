package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/recommendation"
	"github.com/pg-sage/sidecar/internal/store"
)

// policy-04: an operator approval outside the maintenance window is
// scheduled for the next window. The runner claims due rows (one sidecar
// wins), re-authorizes them under the current policy and runs them; any
// other outcome returns the row to pending with the reason. Real
// PostgreSQL: a real standing gate, queue and recommendation store.

const scheduleApprover = 7

// scheduleClock is the standing gate's clock: a Wednesday 14:00 UTC at
// least a week ahead of the real clock, so the planned run_after is never
// due by the database's now() until a test moves it.
type scheduleClock struct{ at atomic.Int64 }

func newScheduleClock() *scheduleClock {
	c := &scheduleClock{}
	day := time.Now().UTC().Add(7 * 24 * time.Hour)
	for day.Weekday() != time.Wednesday {
		day = day.Add(24 * time.Hour)
	}
	c.set(time.Date(day.Year(), day.Month(), day.Day(), 14, 0, 0, 0, time.UTC))
	return c
}

func (c *scheduleClock) set(at time.Time) { c.at.Store(at.UnixNano()) }
func (c *scheduleClock) now() time.Time   { return time.Unix(0, c.at.Load()).UTC() }

// insideWindow moves the clock to Thursday 02:00, inside the window.
func (c *scheduleClock) insideWindow() {
	wed := c.now()
	c.set(time.Date(wed.Year(), wed.Month(), wed.Day()+1, 2, 0, 0, 0, time.UTC))
}

type scheduleFixture struct {
	*staleFixture
	clock  *scheduleClock
	events *eventCapture
	queued queueRow
}

// staffedUTC is the staffed profile, its window zoned to UTC.
func staffedUTC() policy.Document {
	doc := policy.StaffedProfile()
	doc.MaintenanceWindows = []string{"weekdays 01:00-05:00 UTC"}
	return doc
}

func newScheduleFixture(t *testing.T, doc policy.Document) *scheduleFixture {
	t.Helper()
	pool, ctx := isolatedSageDB(t)
	fx := &scheduleFixture{clock: newScheduleClock(), events: &eventCapture{}}
	fx.staleFixture = newStaleFixtureOn(t, pool, ctx, "advisory", unverifiedDetail(), doc)
	fx.exec.EnableStandingPolicyDocument(doc, fx.clock.now)
	fx.exec.WithDispatcher(fx.events)
	fx.queued = fx.queuePending(t)
	return fx
}

func (fx *scheduleFixture) schedule(t *testing.T) ScheduledApproval {
	t.Helper()
	got, err := fx.exec.ScheduleApproval(fx.ctx, fx.queue, fx.queued.ID, scheduleApprover,
		"")
	if err != nil {
		t.Fatalf("ScheduleApproval: %v", err)
	}
	return got
}

// open makes the scheduled row due by the database clock and moves the
// gate's clock inside the window.
func (fx *scheduleFixture) open(t *testing.T) {
	t.Helper()
	fx.setQueue(t, fx.queued.ID, `run_after = now() - interval '1 minute',
		window_closes_at = now() + interval '1 hour'`)
	fx.clock.insideWindow()
}

func (fx *scheduleFixture) row(t *testing.T) store.QueuedAction {
	t.Helper()
	got, err := fx.queue.GetByID(fx.ctx, fx.queued.ID)
	if err != nil {
		t.Fatalf("get queue row: %v", err)
	}
	return *got
}

func (fx *scheduleFixture) eventSubjects() string {
	fx.events.mu.Lock()
	defer fx.events.mu.Unlock()
	var subjects []string
	for _, event := range fx.events.events {
		subjects = append(subjects, event.Type+": "+event.Subject+" | "+event.Body)
	}
	return strings.Join(subjects, "\n")
}

func TestScheduledApprovalRunsWhenTheWindowOpens(t *testing.T) {
	fx := newScheduleFixture(t, staffedUTC())
	got := fx.schedule(t)
	thursday := fx.clock.now().Add(11 * time.Hour) // Thursday 01:00
	if !got.Start.At.Equal(thursday) || !got.Start.ClosesAt.Equal(thursday.Add(4*time.Hour)) {
		t.Fatalf("start = %+v, want %s until 05:00", got.Start, thursday)
	}
	row := fx.row(t)
	if row.Status != "scheduled" || row.ScheduledBy == nil || *row.ScheduledBy !=
		scheduleApprover || row.RunAfter == nil || !row.RunAfter.Equal(thursday) ||
		row.WindowClosesAt == nil || row.ScheduledAt == nil {
		t.Fatalf("row = %+v, want scheduled by %d for %s", row, scheduleApprover, thursday)
	}
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 {
		t.Fatalf("ran %d before the window opened", n)
	}
	fx.open(t)
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 1 {
		t.Fatalf("RunScheduledApprovals ran %d, want 1", n)
	}
	row = fx.row(t)
	if row.Status != "executed" || row.ActionLogID == nil || row.DecidedBy == nil ||
		*row.DecidedBy != scheduleApprover {
		t.Fatalf("row = %+v, want executed and decided by the approver", row)
	}
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 || !fx.indexExists(t, fx.index()) {
		t.Fatalf("action_log rows = %d, index exists = %v; want one run that built it",
			n, fx.indexExists(t, fx.index()))
	}
	// CHECK-16: the run was authorized inside the window as an operator
	// approval: Apply's authorization and its re-authorization under the
	// change lease, both execute.
	if n := fx.count(t, `SELECT count(*) FROM sage.decision WHERE verdict = 'execute'
		AND reason = 'operator_approved'`); n != 2 {
		t.Fatalf("execute/operator_approved decisions = %d, want 2 (authorize, reauthorize)", n)
	}
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 {
		t.Fatalf("a second pass ran %d; a schedule runs once", n)
	}
}

func TestScheduledApprovalRevisedBeforeOpeningReturnsToPending(t *testing.T) {
	fx := newScheduleFixture(t, staffedUTC())
	fx.schedule(t)
	revised := fx.indexFinding(fx.index()+"_v2", unverifiedDetail())
	fx.setFinding(t, revised.RecommendedSQL, unverifiedDetail())
	res, err := fx.recs.Propose(fx.ctx, analyzer.RecommendationProposal(fx.database, revised))
	if err != nil || res.Outcome != recommendation.OutcomeRevised {
		t.Fatalf("revise: %+v, %v", res, err)
	}
	fx.open(t)
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 {
		t.Fatalf("ran %d revised approvals", n)
	}
	row := fx.row(t)
	if row.Status != "pending" || !strings.Contains(row.Reason, "revised") ||
		row.RunAfter != nil {
		t.Fatalf("row = %+v, want pending, unscheduled, reason naming the revision", row)
	}
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 0 {
		t.Fatalf("the old content ran %d times", n)
	}
	if !strings.Contains(fx.eventSubjects(), "returned to approval") {
		t.Fatalf("approver not notified; events:\n%s", fx.eventSubjects())
	}
}

func TestScheduledApprovalWithheldByAnEmergencyStop(t *testing.T) {
	fx := newScheduleFixture(t, staffedUTC())
	fx.schedule(t)
	fx.exec.WithEmergencyStopCheck(func(context.Context) bool { return true })
	fx.open(t)
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 {
		t.Fatalf("ran %d under an emergency stop", n)
	}
	row := fx.row(t)
	if row.Status != "pending" || !strings.Contains(row.Reason, "emergency stop") {
		t.Fatalf("row = %+v, want pending with the emergency stop reason", row)
	}
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 0 {
		t.Fatalf("ran %d times under an emergency stop", n)
	}
	if !strings.Contains(fx.eventSubjects(), "emergency stop") {
		t.Fatalf("approver not told why; events:\n%s", fx.eventSubjects())
	}
}

func TestScheduledApprovalRefusedByANarrowedPolicy(t *testing.T) {
	fx := newScheduleFixture(t, staffedUTC())
	fx.schedule(t)
	narrowed := staffedUTC()
	narrowed.AllowedChangeClasses = []policy.ChangeClass{policy.ChangeAnalyze}
	fx.exec.EnableStandingPolicyDocument(narrowed, fx.clock.now)
	fx.open(t)
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 {
		t.Fatalf("ran %d after the class was removed", n)
	}
	row := fx.row(t)
	if row.Status != "pending" || !strings.Contains(row.Reason, "change class not allowed") {
		t.Fatalf("row = %+v, want pending with the gate's reason", row)
	}
}

func TestScheduledApprovalMissedWindowReturnsToPending(t *testing.T) {
	fx := newScheduleFixture(t, staffedUTC())
	fx.schedule(t)
	// The sidecar was down for the whole window two days ago.
	fx.setQueue(t, fx.queued.ID, `run_after = now() - interval '2 days',
		window_closes_at = now() - interval '2 days' + interval '4 hours'`)
	fx.clock.insideWindow()
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 {
		t.Fatalf("ran %d after the window closed", n)
	}
	row := fx.row(t)
	if row.Status != "pending" || !strings.Contains(row.Reason, "window closed") {
		t.Fatalf("row = %+v, want pending, reason naming the missed window", row)
	}
	// CHECK-18: no schedule is left more than a day overdue.
	if n := fx.count(t, `SELECT count(*) FROM sage.action_queue WHERE status = 'scheduled'
		AND run_after < now() - interval '1 day'`); n != 0 {
		t.Fatalf("%d schedules overdue by more than a day", n)
	}
}

func TestScheduledApprovalFailureIsRecordedNotRescheduled(t *testing.T) {
	fx := newScheduleFixture(t, staffedUTC())
	fx.schedule(t)
	if _, err := fx.pool.Exec(fx.ctx, "DROP TABLE public."+fx.table); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	fx.open(t)
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 {
		t.Fatalf("RunScheduledApprovals counted %d successful runs", n)
	}
	row := fx.row(t)
	if row.Status != "failed" || row.Reason == "" || row.RunAfter != nil {
		t.Fatalf("row = %+v, want failed with the error and no schedule", row)
	}
	if !strings.Contains(fx.eventSubjects(), "action_failed") {
		t.Fatalf("approver not notified of the failure; events:\n%s", fx.eventSubjects())
	}
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 || fx.row(t).Status != "failed" {
		t.Fatal("a failed schedule was picked up again")
	}
}

func TestScheduledApprovalClaimedOnceByTwoRunners(t *testing.T) {
	fx := newScheduleFixture(t, staffedUTC())
	fx.schedule(t)
	other, err := pgxpool.NewWithConfig(fx.ctx, fx.pool.Config())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(other.Close)
	second := fx.newExecutorOn(t, other)
	second.EnableStandingPolicyDocument(staffedUTC(), fx.clock.now)
	fx.open(t)
	var wg sync.WaitGroup
	var ran atomic.Int64
	for _, runner := range []*Executor{fx.exec, second} {
		wg.Add(1)
		go func(e *Executor) {
			defer wg.Done()
			ran.Add(int64(e.RunScheduledApprovals(fx.ctx)))
		}(runner)
	}
	wg.Wait()
	if ran.Load() != 1 || fx.actions(t, fx.f.RecommendedSQL) != 1 {
		t.Fatalf("runs = %d, action_log rows = %d; want exactly one execution",
			ran.Load(), fx.actions(t, fx.f.RecommendedSQL))
	}
}

func TestScheduleApprovalRefusals(t *testing.T) {
	fx := newScheduleFixture(t, staffedUTC())
	fx.clock.insideWindow()
	_, err := fx.exec.ScheduleApproval(fx.ctx, fx.queue, fx.queued.ID, scheduleApprover, "")
	if !errors.Is(err, ErrApprovalRunsNow) {
		t.Fatalf("schedule inside the window: %v, want ErrApprovalRunsNow", err)
	}
	fx.clock.set(fx.clock.now().Add(12 * time.Hour)) // Thursday 14:00
	narrowed := staffedUTC()
	narrowed.AllowedChangeClasses = []policy.ChangeClass{policy.ChangeAnalyze}
	fx.exec.EnableStandingPolicyDocument(narrowed, fx.clock.now)
	_, err = fx.exec.ScheduleApproval(fx.ctx, fx.queue, fx.queued.ID, scheduleApprover, "")
	if !errors.Is(err, ErrNotWindowOnly) || !strings.Contains(err.Error(), "change class") {
		t.Fatalf("schedule a class refusal: %v, want ErrNotWindowOnly naming it", err)
	}
	if row := fx.row(t); row.Status != "pending" {
		t.Fatalf("a refused schedule changed the row: %+v", row)
	}
	fx.exec.EnableStandingPolicyDocument(staffedUTC(), fx.clock.now)
	_, err = fx.exec.ScheduleApproval(fx.ctx, fx.queue, fx.queued.ID, scheduleApprover,
		"CREATE INDEX CONCURRENTLY other ON public.x (y)")
	if !errors.Is(err, store.ErrNotSchedulable) {
		t.Fatalf("schedule other content: %v, want store.ErrNotSchedulable", err)
	}
	fx.schedule(t)
	_, err = fx.exec.ScheduleApproval(fx.ctx, fx.queue, fx.queued.ID, scheduleApprover, "")
	if err == nil {
		t.Fatal("a scheduled row was scheduled again")
	}
}

func TestUnscheduleReturnsTheRowToPending(t *testing.T) {
	fx := newScheduleFixture(t, staffedUTC())
	fx.schedule(t)
	got, err := fx.queue.Unschedule(fx.ctx, fx.queued.ID, scheduleApprover)
	if err != nil {
		t.Fatalf("Unschedule: %v", err)
	}
	if got.Status != "pending" || got.RunAfter != nil || got.ScheduledBy != nil ||
		!strings.Contains(got.Reason, "cancelled") {
		t.Fatalf("unscheduled row = %+v, want pending with the schedule cleared", got)
	}
	if _, err := fx.queue.Unschedule(fx.ctx, fx.queued.ID, scheduleApprover); !errors.Is(err,
		store.ErrNotScheduled) {
		t.Fatalf("Unschedule of a pending row: %v, want store.ErrNotScheduled", err)
	}
	fx.open(t)
	if n := fx.exec.RunScheduledApprovals(fx.ctx); n != 0 {
		t.Fatalf("ran %d cancelled schedules", n)
	}
}

func TestScheduleApprovalAlwaysWindowRunsNow(t *testing.T) {
	// Nil/empty windows: "always" means an approval runs at once, as today.
	doc := staffedUTC()
	doc.MaintenanceWindows = []string{"always"}
	fx := newScheduleFixture(t, doc)
	_, err := fx.exec.ScheduleApproval(fx.ctx, fx.queue, fx.queued.ID, scheduleApprover, "")
	if !errors.Is(err, ErrApprovalRunsNow) {
		t.Fatalf("schedule with an always window: %v, want ErrApprovalRunsNow", err)
	}
}

func TestScheduledApprovalsSkipWithoutAPool(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	if n := exec.RunScheduledApprovals(context.Background()); n != 0 {
		t.Fatalf("ran %d without a pool", n)
	}
}

// newExecutorOn is the fixture's executor shape on another pool.
func (fx *scheduleFixture) newExecutorOn(t *testing.T, pool *pgxpool.Pool) *Executor {
	t.Helper()
	saved := fx.pool
	fx.pool = pool
	defer func() { fx.pool = saved }()
	exec := fx.staleFixture.newExecutor(t)
	exec.WithActionStore(store.NewActionStore(pool), "auto")
	return exec
}

func (fx *scheduleFixture) count(t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}
