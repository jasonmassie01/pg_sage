package tuner

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/planfresh"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// tuning-08: the tuner reads a candidate's plan once per cycle, and only a
// plan newer than the relations' last ANALYZE, pg_sage's last action on them
// and tuner.plan_max_age_minutes.

// cacheReads counts statements that read sage.explain_cache.
type cacheReads struct{ n atomic.Int64 }

func (c *cacheReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn,
	d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "FROM sage.explain_cache") {
		c.n.Add(1)
	}
	return ctx
}

func (c *cacheReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func countingPool(t *testing.T) (*pgxpool.Pool, *cacheReads) {
	t.Helper()
	requireTunerDB(t)
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	counter := &cacheReads{}
	cfg.ConnConfig.Tracer = counter
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, counter
}

// planTable is a fresh schema with an analyzed table t; cleanup drops it
// and the queryid's cached plans.
func planTable(t *testing.T, pool *pgxpool.Pool, qid int64) string {
	t.Helper()
	ctx := context.Background()
	s := fmt.Sprintf("tpf_%d", time.Now().UnixNano()%1_000_000_000)
	for _, sql := range []string{"CREATE SCHEMA " + s,
		"CREATE TABLE " + s + ".t (id int PRIMARY KEY, v int)",
		"INSERT INTO " + s + ".t SELECT g, g FROM generate_series(1, 200) g",
		"ANALYZE " + s + ".t"} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.explain_cache WHERE queryid = $1", qid)
		_, _ = pool.Exec(bg, "DROP SCHEMA IF EXISTS "+s+" CASCADE")
	})
	return s
}

func diskSortOn(s string) string {
	return `[{"Plan":{"Node Type":"Sort","Sort Space Type":"Disk","Sort Space Used":9000,` +
		`"Plan Rows":1000,"Plans":[{"Node Type":"Seq Scan","Relation Name":"t",` +
		`"Schema":"` + s + `","Plan Rows":1000}]}}]`
}

func cachePlan(t *testing.T, pool *pgxpool.Pool, qid int64, ago, plan string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.explain_cache
		(queryid, captured_at, plan_json, source) VALUES ($1, now() - $2::interval, $3,
		'auto_explain')`, qid, ago, plan); err != nil {
		t.Fatalf("cache plan: %v", err)
	}
}

func hasKind(syms []PlanSymptom, k SymptomKind) bool {
	for _, s := range syms {
		if s.Kind == k {
			return true
		}
	}
	return false
}

// CHECK-12: one explain_cache read per candidate in a cycle.
func TestTuneCandidates_ReadsThePlanOncePerCandidate(t *testing.T) {
	pool, reads := countingPool(t)
	qid := -time.Now().UnixNano()
	s := planTable(t, pool, qid)
	cachePlan(t, pool, qid, "0 seconds", diskSortOn(s))
	tu := New(pool, TunerConfig{WorkMemMaxMB: 512}, &HintPlanAvailability{}, noopLogFn)
	c := candidate{QueryID: qid, Query: "SELECT * FROM " + s + ".t ORDER BY v",
		Calls: 500, MeanExecTime: 500}
	reads.n.Store(0)
	findings, err := tu.tuneCandidates(context.Background(), []candidate{c},
		map[string]bool{"other.table": true})
	if err != nil {
		t.Fatalf("tuneCandidates: %v", err)
	}
	if got := reads.n.Load(); got != 1 {
		t.Fatalf("explain_cache reads for one candidate = %d, want 1", got)
	}
	if len(findings) == 0 {
		t.Fatal("the disk-sort plan must still produce a finding")
	}
	plan, ok := findings[0].Detail["plan"].(map[string]any)
	if !ok || plan["source"] != planfresh.SourceCache || plan["captured_at"] == "" ||
		plan["captured_at"] == nil || plan["age_seconds"] == nil {
		t.Fatalf("finding detail must carry the plan's source, captured_at and age: %v",
			findings[0].Detail)
	}
}

// CHECK-13: after ANALYZE t the older cached plan for t is ignored.
func TestTuner_IgnoresCachedPlanOlderThanAnalyze(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	qid := -time.Now().UnixNano()
	s := planTable(t, pool, qid)
	cachePlan(t, pool, qid, "0 seconds", diskSortOn(s))
	tu := New(pool, TunerConfig{}, nil, noopLogFn)
	if syms := tu.scanPlanForQuery(ctx, qid); !hasKind(syms, SymptomDiskSort) {
		t.Fatalf("a plan newer than ANALYZE is used: %v", syms)
	}
	analyzeVisible(t, pool, s)
	if syms := tu.scanPlanForQuery(ctx, qid); len(syms) != 0 {
		t.Fatalf("after ANALYZE the older plan must be ignored, got %v", syms)
	}
	if got := tu.fetchPlanJSON(ctx, qid); got != "" {
		t.Fatalf("fetchPlanJSON after ANALYZE = %q, want none", got)
	}
}

func analyzeVisible(t *testing.T, pool *pgxpool.Pool, s string) {
	t.Helper()
	ctx := context.Background()
	var mark time.Time
	if err := pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&mark); err != nil {
		t.Fatalf("clock: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE "+s+".t"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var seen bool
		if err := pool.QueryRow(ctx, `SELECT COALESCE(last_analyze > $2, false)
			FROM pg_stat_user_tables WHERE schemaname = $1 AND relname = 't'`, s, mark).
			Scan(&seen); err == nil && seen {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("ANALYZE of %s.t never became visible", s)
}

func TestTuner_PlanOlderThanMaxAgeIsIgnored(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	qid := -time.Now().UnixNano()
	s := planTable(t, pool, qid)
	cachePlan(t, pool, qid, "-1 second", diskSortOn(s))
	if syms := New(pool, TunerConfig{}, nil, noopLogFn).scanPlanForQuery(ctx, qid); !hasKind(
		syms, SymptomDiskSort) {
		t.Fatalf("within the 24h default the plan is used: %v", syms)
	}
	qid2 := qid - 1
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_cache WHERE queryid=$1",
			qid2)
	})
	cachePlan(t, pool, qid2, "25 hours", `[{"Plan":{"Node Type":"Sort",`+
		`"Sort Space Type":"Disk","Sort Space Used":9000}}]`)
	if syms := New(pool, TunerConfig{}, nil, noopLogFn).scanPlanForQuery(ctx, qid2); len(
		syms) != 0 {
		t.Fatalf("a 25h-old plan is past the default limit: %v", syms)
	}
	short := New(pool, TunerConfig{PlanMaxAgeMinutes: 60 * 26}, nil, noopLogFn)
	if syms := short.scanPlanForQuery(ctx, qid2); !hasKind(syms, SymptomDiskSort) {
		t.Fatalf("plan_max_age_minutes=1560 keeps a 25h plan: %v", syms)
	}
}

// State transition: a plain-EXPLAIN plan (no actuals) never yields the
// spill or nested-loop rules, even when its JSON carries their fields.
func TestTuner_PlainExplainPlanNeverYieldsActualsRules(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	qid := -time.Now().UnixNano()
	planTable(t, pool, qid)
	cachePlan(t, pool, qid, "30 hours", `[{"Plan":{"Node Type":"Index Scan"}}]`)
	explained := `[{"Plan":{"Node Type":"Nested Loop","Plan Rows":1,"Actual Rows":90000,` +
		`"Actual Loops":1,"Plans":[{"Node Type":"Sort","Sort Space Type":"Disk",` +
		`"Sort Space Used":9000},{"Node Type":"Hash","Hash Batches":8}]}}]`
	var calls int
	tu := New(pool, TunerConfig{}, nil, noopLogFn).WithPlanSource(
		func(context.Context, string) (planfresh.Plan, error) {
			calls++
			return planfresh.Plan{Source: planfresh.SourceExplain, JSON: []byte(explained)}, nil
		}, catalogread.Default())
	syms := tu.scanPlanForQuery(ctx, qid)
	if calls != 1 {
		t.Fatalf("a stale cached plan must be replaced by one EXPLAIN, calls = %d", calls)
	}
	for _, k := range []SymptomKind{SymptomDiskSort, SymptomHashSpill, SymptomBadNestedLoop} {
		if hasKind(syms, k) {
			t.Fatalf("plain EXPLAIN yielded %s: %v", k, syms)
		}
	}
}

func TestTuner_NoCachedPlanRunsNoExplain(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	var calls int
	tu := New(pool, TunerConfig{}, nil, noopLogFn).WithPlanSource(
		func(context.Context, string) (planfresh.Plan, error) {
			calls++
			return planfresh.Plan{}, nil
		}, catalogread.Default())
	if syms := tu.scanPlanForQuery(ctx, -time.Now().UnixNano()); len(syms) != 0 ||
		calls != 0 {
		t.Fatalf("without a cached plan the tuner keeps today's no-plan path: %v (%d)",
			syms, calls)
	}
}
