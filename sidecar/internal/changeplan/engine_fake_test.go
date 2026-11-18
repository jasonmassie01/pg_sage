package changeplan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// The engine's test doubles (plan-02a). The queue, the plan rows and the
// action_log rows are real (a fresh database per fixture); the executor's
// run and rollback, the in-flight lookups and the verifier are fakes, so a
// test decides every verdict and every refusal. engine_db_test.go runs the
// real executor.

// fakeClock is the engine's and the runner's clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// manualCall is one ExecuteManual the plan runner made.
type manualCall struct {
	findingID int
	sql       string
	approver  int
	actionID  int64
	at        time.Time
}

// fakeManual stands in for executor.ExecuteManual: it records an
// action_log row (outcome monitoring) unless refuse names the SQL.
type fakeManual struct {
	mu     sync.Mutex
	pool   *pgxpool.Pool
	clock  *fakeClock
	refuse map[string]string
	calls  []manualCall
	// crash, when set, runs before the ledger row ("before") and after it
	// ("after"): a panic there is the sidecar killed mid-run (plan-02e).
	crash func(stage string)
}

func (m *fakeManual) ExecuteManual(ctx context.Context, findingID int, sql,
	rollbackSQL string, approvedBy *int) (int64, error) {
	m.mu.Lock()
	reason, refused := m.refuse[sql]
	m.mu.Unlock()
	if refused {
		return 0, fmt.Errorf("policy refused operator action: %s", reason)
	}
	m.crashAt("before")
	var id int64
	err := m.pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, finding_id,
		sql_executed, rollback_sql, outcome, executed_at)
		VALUES ('create_index', $1, $2, NULLIF($3, ''), 'monitoring', $4) RETURNING id`,
		findingID, sql, rollbackSQL, m.clock.Now()).Scan(&id)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	approver := 0
	if approvedBy != nil {
		approver = *approvedBy
	}
	m.calls = append(m.calls, manualCall{findingID: findingID, sql: sql, approver: approver,
		actionID: id, at: m.clock.Now()})
	if m.crash != nil {
		defer m.crash("after")
	}
	return id, nil
}

func (m *fakeManual) crashAt(stage string) {
	m.mu.Lock()
	crash := m.crash
	m.mu.Unlock()
	if crash != nil {
		crash(stage)
	}
}

func (m *fakeManual) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// fakeExec stands in for the executor: RunApprovedAction routes plan_step:
// items to the registered plan runner (as the real executor does), and
// RollbackAction marks the action rolled back.
type fakeExec struct {
	mu          sync.Mutex
	pool        *pgxpool.Pool
	runner      executor.ApprovedActionRunner
	runErr      error
	rollbacks   []int64
	rollbackErr error
	// rollbackFailSQL fails the rollback of the action that ran that SQL
	// (plan-02c's walk tests).
	rollbackFailSQL map[string]error
	// afterRollback runs after each rollback the fake records.
	afterRollback func()
	// beforeRun, when set, runs first in RunApprovedAction (plan-02e's
	// kill between a step's approval and its run).
	beforeRun func()
}

func (f *fakeExec) RunApprovedAction(ctx context.Context, a store.QueuedAction,
	approvedBy int) (executor.ApprovedRun, error) {
	f.mu.Lock()
	runErr, runner, before := f.runErr, f.runner, f.beforeRun
	f.mu.Unlock()
	if before != nil {
		before()
	}
	if runErr != nil {
		return executor.ApprovedRun{}, runErr
	}
	if !strings.HasPrefix(a.IdentityKey, executor.PlanStepIdentityPrefix) ||
		runner == nil || !runner.Owns(a) {
		return executor.ApprovedRun{}, executor.ErrPlanStepOutsidePlan
	}
	return runner.RunApproved(ctx, a, approvedBy)
}

func (f *fakeExec) RollbackAction(ctx context.Context, actionID int64, _ string) error {
	f.mu.Lock()
	f.rollbacks = append(f.rollbacks, actionID)
	err, failSQL, after := f.rollbackErr, f.rollbackFailSQL, f.afterRollback
	f.mu.Unlock()
	if after != nil {
		defer after()
	}
	if err != nil {
		return err
	}
	if len(failSQL) > 0 {
		var sql string
		if err := f.pool.QueryRow(ctx, `SELECT sql_executed FROM sage.action_log
			WHERE id = $1`, actionID).Scan(&sql); err != nil {
			return err
		}
		if err := failSQL[sql]; err != nil {
			return err
		}
	}
	_, err = f.pool.Exec(ctx, `UPDATE sage.action_log SET outcome = 'rolled_back'
		WHERE id = $1`, actionID)
	return err
}

func (f *fakeExec) rollbackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rollbacks)
}

// fakeWaits answers the in-flight lookups.
type fakeWaits struct {
	mu      sync.Mutex
	pending map[string]bool // proposed SQL whose objects are busy
	anyBusy bool
	err     error
}

func (w *fakeWaits) PendingFor(_ context.Context, sql string,
	_ []string) ([]policy.PendingVerification, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return nil, w.err
	}
	if w.pending[sql] {
		return []policy.PendingVerification{{ActionID: 999, Object: "table:public.orders"}},
			nil
	}
	return nil, nil
}

func (w *fakeWaits) AnyInFlight(context.Context) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.anyBusy, w.err
}

// fakeVerdicts is the verifier: a test sets each action's verdict.
type fakeVerdicts struct {
	mu  sync.Mutex
	by  map[int64]ActionVerdict
	err error
}

func (v *fakeVerdicts) Verdict(_ context.Context, actionID int64) (ActionVerdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.err != nil {
		return ActionVerdict{}, v.err
	}
	got, ok := v.by[actionID]
	if !ok {
		return ActionVerdict{ActionOutcome: "monitoring"}, nil
	}
	return got, nil
}

func (v *fakeVerdicts) set(actionID int64, verdict string, at time.Time, actionOutcome string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.by[actionID] = ActionVerdict{Verdict: verdict, DecidedAt: &at,
		ActionOutcome: actionOutcome}
}

// fakeNotify records the engine's notifications.
type fakeNotify struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *fakeNotify) Dispatch(_ context.Context, evt notify.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, evt)
	return nil
}

func (n *fakeNotify) sent() []notify.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]notify.Event(nil), n.events...)
}

// engineFixture is one engine over a fresh database with every fake.
type engineFixture struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	store    *Store
	clock    *fakeClock
	manual   *fakeManual
	exec     *fakeExec
	waits    *fakeWaits
	verdicts *fakeVerdicts
	notes    *fakeNotify
	stopped  *atomic.Bool
	leader   *atomic.Bool
	engine   *Engine
	// caps are the plan caps the store and the engines read (plan-02b).
	// plan-02a's engine tests are about sequencing, so the fixture allows
	// 10 new indexes per table (their chains build 3 on one table);
	// caps_db_test.go sets the policy defaults. sizes, when set, replaces
	// the database's live sizes.
	caps    policy.PlanCaps
	capsErr error
	sizes   *fakeSizes
}

func newEngineFixture(t *testing.T, label string) *engineFixture {
	t.Helper()
	pool := planPool(t, label)
	f := &engineFixture{t: t, ctx: context.Background(), pool: pool,
		caps:     policy.PlanCaps{MaxNewIndexesPerTable: 10},
		clock:    &fakeClock{now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)},
		waits:    &fakeWaits{pending: map[string]bool{}},
		verdicts: &fakeVerdicts{by: map[int64]ActionVerdict{}},
		notes:    &fakeNotify{}, stopped: &atomic.Bool{}, leader: &atomic.Bool{}}
	f.leader.Store(true)
	f.store = NewStore(pool).WithCaps(f.readCaps)
	// The resolver's findings, open, so their queue items are approvable.
	if _, err := pool.Exec(f.ctx, `INSERT INTO sage.findings (id, category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql)
		SELECT g, 'plan_engine_' || g, 'warning', 'index', 'public.orders', 'f', '{}',
		       'rec', 'CREATE INDEX CONCURRENTLY x ON public.orders (a)'
		  FROM generate_series(11, 13) g`); err != nil {
		t.Fatalf("insert findings: %v", err)
	}
	f.manual = &fakeManual{pool: pool, clock: f.clock, refuse: map[string]string{}}
	f.exec = &fakeExec{pool: pool}
	f.engine = f.newEngine("engine-a", f.leader)
	return f
}

// newEngine is another engine (another sidecar, with its own executor
// routing to its own runner) on the fixture's database. The fixture's
// first engine uses f.exec.
func (f *engineFixture) newEngine(owner string, leader *atomic.Bool) *Engine {
	f.t.Helper()
	ex := f.exec
	if ex == nil || ex.runner != nil {
		ex = &fakeExec{pool: f.pool}
	}
	e, err := NewEngine(EngineConfig{Pool: f.pool, Executor: ex, Waits: f.waits,
		Verdicts: f.verdicts, Notify: f.notes, Leader: leader.Load, Now: f.clock.Now,
		Owner: owner, Database: "db1", Caps: f.readCaps, Sizes: fixtureSizes{f},
		Stopped: func(context.Context) bool { return f.stopped.Load() }})
	if err != nil {
		f.t.Fatalf("NewEngine: %v", err)
	}
	ex.runner = e.Runner(f.manual)
	return e
}

// approved creates p and approves it as user 7.
func (f *engineFixture) approved(p Plan) Plan {
	f.t.Helper()
	created := mustCreate(f.t, f.store, p)
	if _, err := f.store.Approve(f.ctx, created.ID, created.PlanHash, 7); err != nil {
		f.t.Fatalf("approve plan %d: %v", created.ID, err)
	}
	return f.get(created.ID)
}

func (f *engineFixture) get(id int64) Plan {
	f.t.Helper()
	p, err := f.store.Get(f.ctx, id)
	if err != nil {
		f.t.Fatalf("get plan %d: %v", id, err)
	}
	return p
}

func (f *engineFixture) tick() {
	f.t.Helper()
	if err := f.engine.Tick(f.ctx); err != nil {
		f.t.Fatalf("tick: %v", err)
	}
}

// judgeAll gives every judging step of plan id the verdict, decided now.
func (f *engineFixture) judgeAll(id int64, verdict, actionOutcome string) int {
	f.t.Helper()
	n := 0
	for _, s := range f.get(id).Steps {
		if s.Runtime.State == StateJudging && s.Runtime.ActionLogID != nil {
			f.verdicts.set(*s.Runtime.ActionLogID, verdict, f.clock.Now(), actionOutcome)
			n++
		}
	}
	return n
}

// runUntil ticks (a minute apart) until plan id is terminal or halted,
// judging each step improved as soon as it is judging.
func (f *engineFixture) runUntil(id int64, verdict string, maxTicks int) Plan {
	f.t.Helper()
	for range maxTicks {
		f.judgeAll(id, verdict, "monitoring")
		f.tick()
		f.clock.Add(time.Minute)
		if p := f.get(id); p.Status.Terminal() || p.Status == StatusHalted {
			return p
		}
	}
	return f.get(id)
}

func (f *engineFixture) events(id int64) []Event {
	f.t.Helper()
	ev, err := f.store.Events(f.ctx, id)
	if err != nil {
		f.t.Fatalf("events of plan %d: %v", id, err)
	}
	return ev
}

func countKind(events []Event, kind EventKind) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// queueItems are the plan's queue items, by id.
func (f *engineFixture) queueItems(planID int64) []store.QueuedAction {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT id FROM sage.action_queue
		WHERE identity_key LIKE $1 ORDER BY id`, fmt.Sprintf("plan_step:%d:%%", planID))
	if err != nil {
		f.t.Fatalf("queue items: %v", err)
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			f.t.Fatalf("queue items: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	as := store.NewActionStore(f.pool)
	out := make([]store.QueuedAction, 0, len(ids))
	for _, id := range ids {
		a, err := as.GetByID(f.ctx, id)
		if err != nil {
			f.t.Fatalf("queue item %d: %v", id, err)
		}
		out = append(out, *a)
	}
	return out
}

// chain is n index steps on queryid target, each depending on the last.
func chain(n int, target int64) Plan {
	p := threeStepPlan()
	p.Steps, p.Edges = nil, nil
	for i := 1; i <= n; i++ {
		s := indexStep(i, fmt.Sprintf("idx_%d_%d", target, i), fmt.Sprintf("c%d", i),
			int64(10+i%3+1), target)
		s.DeadlineMinutes = 60
		p.Steps = append(p.Steps, s)
		if i > 1 {
			p.Edges = append(p.Edges, Edge{From: i - 1, To: i})
		}
	}
	p.Scope.QueryIDs = []int64{target}
	return p
}

var errBoom = errors.New("boom: lock timeout on public.orders")
