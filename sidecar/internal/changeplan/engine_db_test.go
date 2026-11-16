package changeplan

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/verify"
)

// plan-02a against the real executor: a two-step plan of index creates on
// two fixture tables runs through RunApprovedAction, the plan runner,
// ExecuteManual (Apply, the standing gate, CREATE INDEX CONCURRENTLY) and
// the ledger's verdicts (verify.OutcomeStore), with the verifier's window
// at 5 minutes (SAGE_VERIFY_WINDOW_MINUTES=5). The verdicts are recorded
// through the outcome store as the verifier records them: the window's
// own judgement takes 5 minutes of workload this test does not have.

type realPlan struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	exec   *executor.Executor
	engine *Engine
	store  *Store
}

func newRealPlan(t *testing.T) *realPlan { return newRealPlanOn(t, "engine_real") }

// newRealPlanOn is newRealPlan on a fresh database named for label.
func newRealPlanOn(t *testing.T, label string) *realPlan {
	t.Helper()
	pool := planPool(t, label)
	ctx := context.Background()
	if err := schema.MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("config schema: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Safety.DDLTimeoutSeconds = 60
	cfg.Verify.WindowMinutes = 5
	ex := executor.New(pool, cfg, time.Time{}, func(string, string, ...any) {})
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	ex.EnableStandingPolicyDocument(doc, nil)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = ex.Shutdown(c)
	})
	dbNow := func() time.Time { // the database's clock, as action_log records it
		var now time.Time
		if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			t.Errorf("read the database clock: %v", err)
		}
		return now
	}
	engine, err := NewEngine(EngineConfig{Pool: pool, Executor: ex,
		Waits: ex.VerificationWaits(), Verdicts: NewDBVerdicts(pool), Now: dbNow,
		Owner: "sidecar-1", Database: "orders"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if err := ex.AddApprovedActionRunner("plans", []string{executor.PlanStepIdentityPrefix},
		engine.Runner(ex)); err != nil {
		t.Fatalf("register the plan runner: %v", err)
	}
	return &realPlan{ctx: ctx, pool: pool, exec: ex, engine: engine, store: NewStore(pool)}
}

// fixture creates the two tables and an open finding for each index.
func (r *realPlan) fixture(t *testing.T) Plan {
	t.Helper()
	if _, err := r.pool.Exec(r.ctx, `CREATE TABLE public.orders AS
		SELECT g AS id, g % 97 AS customer_id FROM generate_series(1, 2000) g;
		CREATE TABLE public.customers AS
		SELECT g AS id, now() - make_interval(mins => g) AS created_at
		  FROM generate_series(1, 2000) g`); err != nil {
		t.Fatalf("fixture tables: %v", err)
	}
	p := threeStepPlan()
	p.Scope.Objects = []string{"public.orders", "public.customers"}
	a := indexStep(1, "idx_plan_a", "customer_id", 0, 101)
	b := indexStep(2, "idx_plan_b", "created_at", 0, 102)
	b.ProposedSQL = "CREATE INDEX CONCURRENTLY idx_plan_b ON public.customers (created_at)"
	b.RollbackSQL = "DROP INDEX CONCURRENTLY public.idx_plan_b"
	for _, s := range []*Step{&a, &b} {
		var id int64
		if err := r.pool.QueryRow(r.ctx, `INSERT INTO sage.findings (category, severity,
			object_type, object_identifier, title, detail, recommendation, recommended_sql)
			VALUES ('plan_real_' || md5($1), 'warning', 'index', 'public.t', 'f', '{}',
			        'rec', $1) RETURNING id`, s.ProposedSQL).Scan(&id); err != nil {
			t.Fatalf("finding: %v", err)
		}
		s.FindingID = ptr(id)
	}
	p.Steps, p.Edges = []Step{a, b}, []Edge{{From: 1, To: 2}}
	res := newFakeResolver()
	res.findings[*a.FindingID], res.findings[*b.FindingID] = true, true
	created, err := r.store.Create(r.ctx, p, res)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.store.Approve(r.ctx, created.ID, created.PlanHash, 7); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return created
}

func (r *realPlan) tick(t *testing.T) {
	t.Helper()
	if err := r.engine.Tick(r.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func (r *realPlan) indexValid(t *testing.T, name string) bool {
	t.Helper()
	var valid bool
	if err := r.pool.QueryRow(r.ctx, `SELECT COALESCE((SELECT indisvalid FROM pg_index
		WHERE indexrelid = to_regclass($1)), false)`, "public."+name).Scan(&valid); err != nil {
		t.Fatalf("read index %s: %v", name, err)
	}
	return valid
}

// judge records verdict for the plan's judging step through the ledger.
func (r *realPlan) judge(t *testing.T, planID int64, verdict string) int64 {
	t.Helper()
	for _, s := range mustGet(t, r.store, planID).Steps {
		if s.Runtime.State == StateJudging {
			o := verify.Outcome{ActionLogID: *s.Runtime.ActionLogID, Verdict: verdict,
				Reason: "test verdict"}
			if err := verify.NewOutcomeStore(r.pool).RecordVerdict(r.ctx, o); err != nil {
				t.Fatalf("record the verdict: %v", err)
			}
			return *s.Runtime.ActionLogID
		}
	}
	t.Fatalf("plan %d has no judging step", planID)
	return 0
}

func TestEngineReal_TwoIndexStepsRunInOrderThroughTheExecutor(t *testing.T) {
	r := newRealPlan(t)
	p := r.fixture(t)
	r.refusesOutsideTheEngine(t, p)
	r.tick(t) // step 1: queue, approve, RunApprovedAction, CREATE INDEX CONCURRENTLY
	if !r.indexValid(t, "idx_plan_a") || r.indexValid(t, "idx_plan_b") {
		t.Fatal("after the first tick want idx_plan_a built and idx_plan_b not")
	}
	r.tick(t) // judging
	for range 3 {
		r.tick(t) // no verdict yet: step 2 must not start
	}
	if r.indexValid(t, "idx_plan_b") {
		t.Fatal("step 2 ran before step 1's verdict")
	}
	r.judge(t, p.ID, "improved")
	for range 3 {
		r.tick(t) // kept; step 2 starts and runs
	}
	r.judge(t, p.ID, "improved")
	for range 3 {
		r.tick(t)
	}
	got := mustGet(t, r.store, p.ID)
	if got.Status != StatusCompleted || !r.indexValid(t, "idx_plan_b") {
		t.Fatalf("plan %s, idx_plan_b valid %v: want completed with both indexes",
			got.Status, r.indexValid(t, "idx_plan_b"))
	}
	r.checkLedger(t, got)
}

// refusesOutsideTheEngine approves a plan_step: item for step 2 the way the
// Actions route does: the executor refuses it and nothing runs.
func (r *realPlan) refusesOutsideTheEngine(t *testing.T, p Plan) {
	t.Helper()
	s := p.Steps[1]
	as := store.NewActionStore(r.pool)
	id, err := as.ProposeWithMetadata(r.ctx, nil, int(*s.FindingID), s.ProposedSQL,
		s.RollbackSQL, "moderate", store.ActionProposalMetadata{
			ActionType: s.ActionType, IdentityKey: PlanStepKey(p.ID, s.ID)})
	if err != nil {
		t.Fatalf("queue a stray plan step item: %v", err)
	}
	item, err := as.Approve(r.ctx, id, 7)
	if err != nil {
		t.Fatalf("approve the stray item: %v", err)
	}
	if _, err := r.exec.RunApprovedAction(r.ctx, *item, 7); !errors.Is(err, ErrNotEngineRun) {
		t.Fatalf("Actions-route run of a plan step = %v, want ErrNotEngineRun", err)
	}
	if r.indexValid(t, "idx_plan_b") {
		t.Fatal("the stray approval built idx_plan_b")
	}
	if _, err := r.pool.Exec(r.ctx, `UPDATE sage.action_queue SET status = 'rejected'
		WHERE id = $1`, id); err != nil {
		t.Fatalf("retire the stray item: %v", err)
	}
}

// checkLedger: each step has one action_log row, executed by the plan's
// approver, and (CHECK-10) step 2's action ran no earlier than step 1's
// verdict.
func (r *realPlan) checkLedger(t *testing.T, p Plan) {
	t.Helper()
	var executed [2]time.Time
	for i, s := range p.Steps {
		if s.Runtime.State != StateKept || s.Runtime.ActionLogID == nil {
			t.Fatalf("step %d %+v, want kept with its action", s.Ordinal, s.Runtime)
		}
		var sql string
		var approvedBy *int
		if err := r.pool.QueryRow(r.ctx, `SELECT sql_executed, approved_by, executed_at
			FROM sage.action_log WHERE id = $1`, *s.Runtime.ActionLogID).Scan(&sql,
			&approvedBy, &executed[i]); err != nil {
			t.Fatalf("step %d action: %v", s.Ordinal, err)
		}
		if sql != s.ProposedSQL || approvedBy == nil || *approvedBy != 7 {
			t.Fatalf("step %d action ran %q approved by %v, want the step's SQL by 7",
				s.Ordinal, sql, approvedBy)
		}
	}
	if executed[1].Before(*p.Steps[0].Runtime.VerdictAt) {
		t.Fatalf("CHECK-10: step 2 executed at %v, before step 1's verdict at %v",
			executed[1], *p.Steps[0].Runtime.VerdictAt)
	}
	t.Logf("CHECK-10: step 2 executed %v after step 1's verdict",
		executed[1].Sub(*p.Steps[0].Runtime.VerdictAt))
}
