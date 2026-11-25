package changeplan

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/verify"
)

// The G-T5 gate's fixture (plan-02e): a real executor on a fresh
// database, three tables, a workload that reads each indexed column and
// writes narrow rows to the third table, and plan engines that can be
// killed and restarted. gateWindow is how long the workload runs before
// each step and how long each WAL measurement lasts.
const gateWindow = 6 * time.Second

type gateRun struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	exec  *executor.Executor
	store *Store
	notes *fakeNotify
	// engine is the live engine; its lease is short so a restart takes
	// over its claims within the test.
	engine *Engine
	dbNow  func() time.Time
}

func newGateRun(t *testing.T) *gateRun {
	t.Helper()
	pool := planPool(t, "gate_t5")
	ctx := context.Background()
	if err := schema.MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("config schema: %v", err)
	}
	g := &gateRun{t: t, ctx: ctx, pool: pool, store: NewStore(pool), notes: &fakeNotify{}}
	g.dbNow = func() time.Time {
		var now time.Time
		if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			t.Errorf("read the database clock: %v", err)
		}
		return now
	}
	g.engine = g.newEngine("sidecar-1", 2*time.Second)
	return g
}

// newEngine is one sidecar on the gate's database: its own executor (the
// verification window SAGE_VERIFY_WINDOW_MINUTES=5, at most 15) and its
// plan engine, whose runner that executor routes plan steps to.
func (g *gateRun) newEngine(owner string, lease time.Duration) *Engine {
	g.t.Helper()
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.Safety.DDLTimeoutSeconds = 120
	cfg.Verify.WindowMinutes, cfg.Verify.WindowMaxMinutes = 5, 15
	ex := executor.New(g.pool, cfg, time.Time{}, func(string, string, ...any) {})
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	ex.EnableStandingPolicyDocument(doc, nil)
	g.t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ex.Shutdown(c)
	})
	e, err := NewEngine(EngineConfig{Pool: g.pool, Executor: ex,
		Waits: ex.VerificationWaits(), Verdicts: NewDBVerdicts(g.pool), Notify: g.notes,
		Now: g.dbNow, Owner: owner, Lease: lease, Database: "gate"})
	if err != nil {
		g.t.Fatalf("NewEngine: %v", err)
	}
	if err := ex.AddApprovedActionRunner("plans", []string{executor.PlanStepIdentityPrefix},
		e.Runner(ex)); err != nil {
		g.t.Fatalf("register the plan runner: %v", err)
	}
	g.exec = ex
	return e
}

// tables creates t1(a), t2(b) and t3(k, payload): narrow rows, a 200-byte
// payload, read by payload; pg_stat_statements is readable.
func (g *gateRun) tables() {
	g.t.Helper()
	if _, err := g.pool.Exec(g.ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
		CREATE TABLE public.gate_t1 AS SELECT g AS id, g % 5000 AS a
		  FROM generate_series(1, 60000) g;
		CREATE TABLE public.gate_t2 AS SELECT g AS id, g % 5000 AS b
		  FROM generate_series(1, 60000) g;
		CREATE TABLE public.gate_t3 (k bigserial, payload text NOT NULL);
		INSERT INTO public.gate_t3 (payload)
		  SELECT rpad(md5(g::text), 200, 'x') FROM generate_series(1, 30000) g;
		ANALYZE public.gate_t1, public.gate_t2, public.gate_t3`); err != nil {
		g.t.Fatalf("gate tables: %v", err)
	}
}

// gateStep is an index step on table.column with an open finding.
func (g *gateRun) gateStep(ordinal int, table, column string, target int64) Step {
	g.t.Helper()
	name := "gate_" + table + "_" + column
	s := indexStep(ordinal, name, column, 0, target)
	s.ProposedSQL = "CREATE INDEX CONCURRENTLY " + name + " ON public.gate_" + table +
		" (" + column + ")"
	s.RollbackSQL = "DROP INDEX CONCURRENTLY public." + name
	var id int64
	if err := g.pool.QueryRow(g.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql)
		VALUES ('gate_' || md5($1), 'warning', 'index', 'public.gate', 'f', '{}', 'rec', $1)
		RETURNING id`, s.ProposedSQL).Scan(&id); err != nil {
		g.t.Fatalf("finding: %v", err)
	}
	s.FindingID = ptr(id)
	return s
}

// approvedPlan is the gate's three-step chain, approved all at once
// through the plan-03a store as user 7.
func (g *gateRun) approvedPlan() Plan {
	g.t.Helper()
	g.tables()
	p := threeStepPlan()
	p.Scope.Objects = []string{"public.gate_t1", "public.gate_t2", "public.gate_t3"}
	p.Scope.QueryIDs = []int64{101, 102, 103}
	p.Steps = []Step{g.gateStep(1, "t1", "a", 101), g.gateStep(2, "t2", "b", 102),
		g.gateStep(3, "t3", "payload", 103)}
	p.Edges = []Edge{{From: 1, To: 2}, {From: 2, To: 3}}
	res := newFakeResolver()
	res.calls[103] = 5000
	for _, s := range p.Steps {
		res.findings[*s.FindingID] = true
	}
	created, err := g.store.Create(g.ctx, p, res)
	if err != nil {
		g.t.Fatalf("create: %v", err)
	}
	if _, err := g.store.ApproveWith(g.ctx, created.ID, Decision{PlanHash: created.PlanHash,
		Mode: ModeAll, Via: ViaAPI}, 7); err != nil {
		g.t.Fatalf("approve all: %v", err)
	}
	return created
}

// workload reads a, b and payload and inserts about 200 narrow rows a
// second into t3 until the returned stop is called.
func (g *gateRun) workload() (stop func()) {
	ctx, cancel := context.WithCancel(g.ctx)
	var wg sync.WaitGroup
	loop := func(every time.Duration, sql string, args func(i int) []any) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(every)
			defer tick.Stop()
			for i := 0; ; i++ {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				_, _ = g.pool.Exec(ctx, sql, args(i)...)
			}
		}()
	}
	// The insert's marker follows its first keyword: PostgreSQL 18 drops a
	// comment ahead of the first token from pg_stat_statements' text.
	loop(5*time.Millisecond, `INSERT /* gate */ INTO public.gate_t3 (payload)
		VALUES (rpad(md5(clock_timestamp()::text), 200, 'y'))`,
		func(int) []any { return nil })
	loop(20*time.Millisecond, `/* gate */ SELECT count(*) FROM public.gate_t1 WHERE a = $1`,
		func(i int) []any { return []any{i % 5000} })
	loop(20*time.Millisecond, `/* gate */ SELECT count(*) FROM public.gate_t2 WHERE b = $1`,
		func(i int) []any { return []any{i % 5000} })
	loop(20*time.Millisecond, `/* gate */ SELECT count(*) FROM public.gate_t3
		WHERE payload = $1`, func(i int) []any {
		return []any{"missing-" + time.Duration(i).String()}
	})
	return func() { cancel(); wg.Wait() }
}

// stat sums pg_stat_statements' calls, time and WAL for statements like
// pattern in this database.
func (g *gateRun) stat(pattern string) (calls, ms, wal float64) {
	g.t.Helper()
	if err := g.pool.QueryRow(g.ctx, `SELECT COALESCE(sum(calls), 0)::float8,
		COALESCE(sum(total_exec_time), 0)::float8, COALESCE(sum(wal_bytes), 0)::float8
		FROM pg_stat_statements WHERE query LIKE $1
		  AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		pattern).Scan(&calls, &ms, &wal); err != nil {
		g.t.Fatalf("read pg_stat_statements: %v", err)
	}
	return calls, ms, wal
}

// measure runs the workload for one window and returns the mean time of
// the reads like readPattern and the WAL bytes per t3 insert.
func (g *gateRun) measure(readPattern string) (readMs, walPerInsert float64) {
	g.t.Helper()
	const insert = "INSERT /* gate */ INTO public.gate_t3%"
	rc0, rms0, _ := g.stat(readPattern)
	ic0, _, iw0 := g.stat(insert)
	time.Sleep(gateWindow)
	rc1, rms1, _ := g.stat(readPattern)
	ic1, _, iw1 := g.stat(insert)
	if rc1 <= rc0 || ic1 <= ic0 {
		g.t.Fatalf("the workload did not run: %v reads, %v inserts", rc1-rc0, ic1-ic0)
	}
	return (rms1 - rms0) / (rc1 - rc0), (iw1 - iw0) / (ic1 - ic0)
}

// tickUntil ticks e until step ordinal of plan id is in state want.
func (g *gateRun) tickUntil(e *Engine, id int64, ordinal int, want StepState) Step {
	g.t.Helper()
	for range 20 {
		if err := e.Tick(g.ctx); err != nil {
			g.t.Fatalf("tick: %v", err)
		}
		if s := g.step(id, ordinal); s.Runtime.State == want {
			return s
		}
		time.Sleep(200 * time.Millisecond)
	}
	s := g.step(id, ordinal)
	g.t.Fatalf("step %d is %s (%s), want %s", ordinal, s.Runtime.State,
		s.Runtime.StateReason, want)
	return Step{}
}

func (g *gateRun) step(id int64, ordinal int) Step {
	g.t.Helper()
	for _, s := range mustGet(g.t, g.store, id).Steps {
		if s.Ordinal == ordinal {
			return s
		}
	}
	g.t.Fatalf("plan %d has no step %d", id, ordinal)
	return Step{}
}

// record stores the verifier's verdict for the step's action.
func (g *gateRun) record(s Step, verdict, reason string) {
	g.t.Helper()
	o := verify.Outcome{ActionLogID: *s.Runtime.ActionLogID, Verdict: verdict,
		Reason: reason}
	if err := verify.NewOutcomeStore(g.pool).RecordVerdict(g.ctx, o); err != nil {
		g.t.Fatalf("record the verdict: %v", err)
	}
}
