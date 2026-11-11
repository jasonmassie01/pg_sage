package tuner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// tuning-05 on real PostgreSQL with pg_hint_plan preloaded (CI image).

const (
	hxJoin = `SELECT c.name, o.amount FROM hx.customers c ` +
		`JOIN hx.orders o ON o.customer_id = c.id WHERE o.id < 50`
	hxPoint = `SELECT * FROM hx.orders o WHERE o.id = 42`
	hxParam = `SELECT * FROM hx.orders o WHERE o.id = $1`
)

var hxOpts = CompareOptions{Timeouts: catalogread.Default(), TolerancePct: 10}

// hintExplainDB is the doc's customers/orders schema (docs/hint-verification
// section 4) in schema hx, analyzed over every row so the estimates are the
// same on every run.
func hintExplainDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := connectTunerTestDB(t)
	t.Cleanup(pool.Close)
	ctx := context.Background()
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS hx CASCADE`, `CREATE SCHEMA hx`,
		`CREATE TABLE hx.customers (id int PRIMARY KEY, name text, region text)`,
		`CREATE TABLE hx.orders (id int PRIMARY KEY, customer_id int, amount numeric)`,
		`INSERT INTO hx.customers SELECT g, 'c' || g,
		   (ARRAY['east','west','north','south'])[1 + g % 4]
		   FROM generate_series(1, 10000) g`,
		`INSERT INTO hx.orders SELECT g, 1 + (g * 7919) % 10000, g % 500
		   FROM generate_series(1, 100000) g`,
		`CREATE INDEX orders_customer_idx ON hx.orders (customer_id)`,
		`SET default_statistics_target = 10000`,
		`ANALYZE hx.customers`, `ANALYZE hx.orders`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("hx fixture %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS hx CASCADE`)
	})
	return pool, ctx
}

func serverMajorOf(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n / 10000
}

// queryIDOf is the statement's current queryid (EXPLAIN VERBOSE): the
// fixture is recreated per test and queryids depend on relation OIDs, so
// pg_stat_statements may still hold the text under an older one.
func queryIDOf(t *testing.T, pool *pgxpool.Pool, stmt string) int64 {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), "EXPLAIN (VERBOSE, FORMAT JSON) "+stmt).
		Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var out []struct {
		QueryID int64 `json:"Query Identifier"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 || out[0].QueryID == 0 {
		t.Fatalf("query identifier of %q: %s, %v", stmt, raw, err)
	}
	return out[0].QueryID
}

func rootNodeType(t *testing.T, plan []byte) string {
	t.Helper()
	root, err := parsePlanRoot(plan)
	if err != nil {
		t.Fatalf("plan %s: %v", plan, err)
	}
	return root.NodeType
}

// Invalid input: pg_hint_plan ignores an unknown index, so the plans match
// and the hint is refused as no_effect.
func TestCompareHint_UnknownIndexIsNoEffect(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	cmp, err := CompareHint(ctx, pool, hxJoin, "IndexScan(o idx_missing)", 0, hxOpts)
	if !errors.Is(err, ErrHintRefused) || cmp.Verdict != HintNoEffect {
		t.Fatalf("unknown index: %q, %v, want no_effect refused", cmp.Verdict, err)
	}
	// pg_hint_plan 1.7 plans the same; 1.4-1.6 (PG14-16) fall back to a
	// disabled path. Both are no effect, for different evidence.
	sameOrDisabled := cmp.PlainHash == cmp.HintedHash ||
		strings.Contains(cmp.Reason, "disabled path")
	if cmp.PlainHash == "" || !sameOrDisabled || cmp.PGMajor < 14 {
		t.Fatalf("no_effect evidence: %s, %s -> %s, PG%d", cmp.Reason, cmp.PlainHash,
			cmp.HintedHash, cmp.PGMajor)
	}
}

// Invalid input: a multi-statement text is refused before any EXPLAIN (no
// plan, no transaction; the second statement never runs).
func TestCompareHint_MultiStatementRefusedFirst(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	cmp, err := CompareHint(ctx, pool, hxPoint+"; DROP SCHEMA hx CASCADE", "SeqScan(o)",
		0, hxOpts)
	if !errors.Is(err, ErrHintRefused) || cmp.Verdict != HintInvalidStatement ||
		cmp.PlainHash != "" || cmp.PGMajor != 0 {
		t.Fatalf("multi-statement: %+v, %v", cmp, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hx.orders`).Scan(&n); err != nil ||
		n != 100000 {
		t.Fatalf("hx.orders after the refusal: %d rows, %v", n, err)
	}
}

// Happy path: HashJoin(c o) turns the nested loop into a hash join within
// the 10%% tolerance; both plans and costs are evidence.
func TestCompareHint_HashJoinAdmitted(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	cmp, err := CompareHint(ctx, pool, hxJoin, "HashJoin(c o)", 0, hxOpts)
	if err != nil || cmp.Verdict != HintChanged || cmp.Basis != "plan" {
		t.Fatalf("HashJoin(c o): %+v, %v, want changed", cmp, err)
	}
	if got := rootNodeType(t, cmp.PlainPlan); got != "Nested Loop" {
		t.Fatalf("plain root %q, want Nested Loop", got)
	}
	if got := rootNodeType(t, cmp.HintedPlan); got != "Hash Join" {
		t.Fatalf("hinted root %q, want Hash Join", got)
	}
	if cmp.HintedCost < cmp.PlainCost || cmp.HintedCost > cmp.PlainCost*1.1 ||
		cmp.Generic || cmp.PlainHash == cmp.HintedHash {
		t.Fatalf("costs %.2f -> %.2f, generic %t", cmp.PlainCost, cmp.HintedCost, cmp.Generic)
	}
	ev := cmp.Evidence()
	raw, err := json.Marshal(ev)
	if err != nil || !strings.Contains(string(raw), `"plain_plan":[{"Plan"`) ||
		!strings.Contains(string(raw), `"hinted_plan":[{"Plan"`) {
		t.Fatalf("evidence without both plans: %s, %v", raw, err)
	}
}

// Boundary: a tenfold estimated cost rise is refused.
func TestCompareHint_TenfoldCostRefused(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	cmp, err := CompareHint(ctx, pool, hxPoint, "SeqScan(o)", 0, hxOpts)
	if !errors.Is(err, ErrHintRefused) || cmp.Verdict != HintCostRegression {
		t.Fatalf("SeqScan(o) on a point lookup: %q, %v", cmp.Verdict, err)
	}
	if cmp.HintedCost < 10*cmp.PlainCost {
		t.Fatalf("costs %.2f -> %.2f, want a tenfold rise", cmp.PlainCost, cmp.HintedCost)
	}
}

// Version: a parameterized statement is unverifiable before PG16 (no
// EXPLAIN runs) and is planned with GENERIC_PLAN from PG16.
func TestCompareHint_ParameterizedByVersion(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	major := serverMajorOf(t, pool)
	for _, given := range []int{0, major} {
		cmp, err := CompareHint(ctx, pool, hxParam, "SeqScan(o)", given, hxOpts)
		if major < genericPlanMajor {
			if err != nil || cmp.Verdict != HintUnverifiable || cmp.PlainHash != "" ||
				cmp.PGMajor != major {
				t.Fatalf("PG%d (given %d): %+v, %v, want unverifiable", major, given, cmp, err)
			}
			continue
		}
		if !cmp.Generic || cmp.Verdict != HintCostRegression || cmp.PGMajor != major ||
			!errors.Is(err, ErrHintRefused) {
			t.Fatalf("PG%d (given %d): %+v, %v, want a GENERIC_PLAN cost_regression",
				major, given, cmp, err)
		}
	}
}

// Error propagation: an EXPLAIN that hits lock_timeout refuses with
// explain_timeout, never admits.
func TestCompareHint_ExplainTimeoutRefuses(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Rollback(context.Background()) }()
	if _, err := locker.Exec(ctx, `LOCK TABLE hx.orders IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	opts := hxOpts
	opts.Timeouts = catalogread.Timeouts{Statement: 5 * time.Second,
		Lock: 100 * time.Millisecond}
	start := time.Now()
	cmp, err := CompareHint(ctx, pool, hxJoin, "HashJoin(c o)", serverMajorOf(t, pool), opts)
	if !errors.Is(err, ErrHintRefused) || cmp.Verdict != HintExplainTimeout ||
		!strings.Contains(cmp.Reason, "plain EXPLAIN") {
		t.Fatalf("locked table: %+v, %v, want explain_timeout", cmp, err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("the lock wait was not bounded: %s", time.Since(start))
	}
}

// Error propagation: pg_hint_plan not loaded (stood in by a module name
// that is never loaded) is unverifiable with or without a session LOAD, and
// CheckHint keeps the finding without recommended_sql.
func TestCompareHint_ModuleNotLoadedIsUnverifiable(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	saved := hintModuleGUC
	hintModuleGUC = "pg_hint_plan_absent.enable_hint"
	t.Cleanup(func() { hintModuleGUC = saved })
	for _, load := range []bool{false, true} {
		opts := hxOpts
		opts.SessionLoad = load
		cmp, err := CompareHint(ctx, pool, hxJoin, "HashJoin(c o)", 0, opts)
		if err != nil || cmp.Verdict != HintUnverifiable || cmp.PlainHash != "" ||
			!strings.Contains(cmp.Reason, "not loaded") {
			t.Fatalf("session load %t: %+v, %v, want unverifiable", load, cmp, err)
		}
	}
}

// The same through CheckHint: the finding is kept without recommended_sql.
func TestCheckHint_ModuleNotLoadedKeepsFindingAdvisory(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	const qid = 9_050_101
	tu := hintTableTuner(t, pool, qid)
	saved := hintModuleGUC
	hintModuleGUC = "pg_hint_plan_absent.enable_hint"
	t.Cleanup(func() { hintModuleGUC = saved })
	f, err := tu.CheckHint(ctx, HintProposal{QueryID: qid, Query: hxJoin, Hint: "HashJoin(c o)"})
	ev, _ := f.Detail["hint_evidence"].(map[string]any)
	if err != nil || f.RecommendedSQL != "" || f.RollbackSQL != "" ||
		ev["verdict"] != "unverifiable" || f.Category != "query_tuning" {
		t.Fatalf("CheckHint without the module: sql %q detail %v, %v", f.RecommendedSQL,
			f.Detail, err)
	}
}

// gatedTuner is a tuner with ready hint facts and the gate; its
// sage.query_hints rows (and hint_plan.hints rows, where the 1.7 table is)
// for ids are removed after the test.
func gatedTuner(t *testing.T, pool *pgxpool.Pool, ids ...int64) *Tuner {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM hint_plan.hints WHERE query_id = ANY($1)`, ids)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.query_hints WHERE queryid = ANY($1)`, ids)
	})
	tu := New(pool, TunerConfig{HintCostTolerancePct: 10, WorkMemMaxMB: 512},
		&HintPlanAvailability{ApplicationEffect: true, HintTableReady: true,
			SharedPreload: true, Preload: true, Privilege: true, HintTableOn: true},
		noopLogFn)
	return tu.WithHintComparison(0, catalogread.Default())
}

// hintTableTuner is gatedTuner over pg_hint_plan 1.7's query_id hint table,
// which CheckHint reads; pg_hint_plan 1.4-1.6 (PG14-16) has none, and there
// hints stay advisory before any comparison.
func hintTableTuner(t *testing.T, pool *pgxpool.Pool, ids ...int64) *Tuner {
	t.Helper()
	requireQueryIDHintTable(t, pool)
	prepareHintTables(t, pool, ids)
	return gatedTuner(t, pool, ids...)
}

// CheckHint (the agent's path): a hint with no effect is refused with no
// finding; a changing hint keeps its SQL and verdict changed (CHECK-28).
func TestCheckHint_GateRefusesAndAdmits(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	const qid = 9_050_102
	tu := hintTableTuner(t, pool, qid)
	f, err := tu.CheckHint(ctx, HintProposal{QueryID: qid, Query: hxJoin,
		Hint: "IndexScan(o idx_missing)"})
	if !errors.Is(err, ErrHintRefused) || f.Category != "" || f.RecommendedSQL != "" {
		t.Fatalf("no_effect hint: %+v, %v", f, err)
	}
	f, err = tu.CheckHint(ctx, HintProposal{QueryID: qid, Query: hxJoin,
		Hint: "HashJoin(c o)"})
	ev, _ := f.Detail["hint_evidence"].(map[string]any)
	if err != nil || f.RecommendedSQL != BuildInsertSQL(qid, "HashJoin(c o)") ||
		ev["verdict"] != "changed" || ev["plain_plan"] == nil || ev["hinted_plan"] == nil {
		t.Fatalf("changed hint: sql %q evidence %v, %v", f.RecommendedSQL, ev, err)
	}
	cmp, err := tu.HintedExplain(ctx, HintProposal{QueryID: qid, Query: hxPoint,
		Hint: "SeqScan(o)"})
	if err != nil || cmp.Verdict != HintCostRegression {
		t.Fatalf("HintedExplain reports a refusal as a verdict: %+v, %v", cmp, err)
	}
}

// Tag placement: through a selfmonitor-tagged pool (the tag after EXPLAIN's
// first keyword) the hint comment stays first and still changes the plan.
func TestCompareHint_TaggedPoolKeepsHintFirst(t *testing.T) {
	plain, ctx := hintExplainDB(t)
	cfg, err := pgxpool.ParseConfig(plain.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	selfmonitor.ConfigurePool(cfg)
	tagged, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tagged.Close()
	cmp, err := CompareHint(ctx, tagged, hxJoin, "HashJoin(c o)", 0, hxOpts)
	if err != nil || cmp.Verdict != HintChanged ||
		rootNodeType(t, cmp.HintedPlan) != "Hash Join" {
		t.Fatalf("tagged pool: %+v, %v", cmp, err)
	}
}

// Concurrency: the agent and the tuner compare in parallel, each in its own
// transaction, and agree. (-race is unavailable on this machine.)
func TestCompareHint_ConcurrentAgentAndTuner(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	const qid = 9_050_103
	tu := gatedTuner(t, pool, qid)
	var wg sync.WaitGroup
	results := make([]HintComparison, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				results[i], errs[i] = CompareHint(ctx, pool, hxJoin, "HashJoin(c o)", 0, hxOpts)
				return
			}
			results[i], errs[i] = tu.HintedExplain(ctx, HintProposal{QueryID: qid,
				Query: hxJoin, Hint: "HashJoin(c o)"})
		}(i)
	}
	wg.Wait()
	for i, r := range results {
		if errs[i] != nil || r.Verdict != HintChanged || r.HintedHash != results[0].HintedHash ||
			r.PlainHash != results[0].PlainHash {
			t.Fatalf("call %d: %+v, %v (first %s/%s)", i, r.Verdict, errs[i],
				results[0].PlainHash, results[0].HintedHash)
		}
	}
}

// An installed hint_plan.hints row for the statement must not plan the
// plain side: with enable_hint_table on for pg_sage's session, a plain
// EXPLAIN takes the row (precondition), yet the comparison still sees the
// unhinted plan and reports the hint as changing it.
func TestCompareHint_InstalledHintRowDoesNotPlanPlainSide(t *testing.T) {
	plain, ctx := hintExplainDB(t)
	if _, err := plain.Exec(ctx, hxJoin); err != nil {
		t.Fatal(err)
	}
	qid := queryIDOf(t, plain, hxJoin)
	requireQueryIDHintTable(t, plain)
	prepareHintTables(t, plain, []int64{qid})
	t.Cleanup(func() {
		_, _ = plain.Exec(context.Background(),
			`DELETE FROM hint_plan.hints WHERE query_id = $1`, qid)
	})
	installHintRow(t, plain, qid, "", "HashJoin(c o)")
	cfg, err := pgxpool.ParseConfig(plain.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["pg_hint_plan.enable_hint_table"] = "on"
	tablePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tablePool.Close()
	var raw []byte
	if err := tablePool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+hxJoin).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if got := rootNodeType(t, raw); got != "Hash Join" {
		t.Fatalf("precondition: the hint table row does not apply (root %q)", got)
	}
	cmp, err := CompareHint(ctx, tablePool, hxJoin, "HashJoin(c o)", 0, hxOpts)
	if err != nil || cmp.Verdict != HintChanged ||
		rootNodeType(t, cmp.PlainPlan) != "Nested Loop" {
		t.Fatalf("installed row planned the plain side: %+v, %v", cmp.Verdict, err)
	}
}

// The deterministic pass: a changing hint passes the gate with its
// comparison (CHECK-28: its finding carries verdict changed); a hint with no
// effect is refused and the statement cools down, so it is not planned again
// next cycle.
func TestCompareHint_DeterministicGate(t *testing.T) {
	pool, ctx := hintExplainDB(t)
	tu := gatedTuner(t, pool)
	c := candidate{QueryID: 9_050_104, Query: hxJoin}
	cmp, refused := tu.gateCandidate(ctx, c, "HashJoin(c o)")
	if refused || cmp == nil || cmp.Verdict != HintChanged {
		t.Fatalf("changing hint: %+v, refused %t", cmp, refused)
	}
	f := tu.buildGatedFinding(ctx, c, nil, "HashJoin(c o)", "t", "r", cmp)
	if ev, _ := f.Detail["hint_evidence"].(map[string]any); ev["verdict"] != "changed" ||
		f.RecommendedSQL == "" {
		t.Fatalf("finding: sql %q detail %v", f.RecommendedSQL, f.Detail)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM sage.query_hints WHERE queryid = $1`, c.QueryID)
	c.QueryID = 9_050_105
	cmp, refused = tu.gateCandidate(ctx, c, "IndexScan(o idx_missing)")
	if !refused || cmp != nil {
		t.Fatalf("no_effect hint passed the gate: %+v", cmp)
	}
	if _, cooling := tu.recentlyTuned[c.QueryID]; !cooling {
		t.Fatal("a refused statement must cool down")
	}
}

// Both sides plan with the session's own max_parallel_workers_per_gather,
// not catalogread's 0: with parallel plans free for this session, the plain
// count(*) is parallel and Set(max_parallel_workers_per_gather "0") changes
// it. Planned with 0 workers, the plain side would match the hinted one and
// the hint would be refused as no_effect.
func TestCompareHint_PlainSideKeepsSessionParallelWorkers(t *testing.T) {
	base, ctx := hintExplainDB(t)
	cfg, err := pgxpool.ParseConfig(base.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"max_parallel_workers_per_gather": "2",
		"parallel_setup_cost": "0", "parallel_tuple_cost": "0",
		"min_parallel_table_scan_size": "0"} {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cmp, err := CompareHint(ctx, pool, `SELECT count(*) FROM hx.orders o`,
		`Set(max_parallel_workers_per_gather "0")`, 0, hxOpts)
	if err != nil || cmp.Verdict != HintChanged || cmp.Basis != "plan" {
		t.Fatalf("parallel workers hint: %+v, %v, want changed", cmp, err)
	}
	if !strings.Contains(string(cmp.PlainPlan), `"Gather`) ||
		strings.Contains(string(cmp.HintedPlan), `"Gather`) {
		t.Fatalf("plain plan should be parallel and hinted not:\n%s\n%s",
			cmp.PlainPlan, cmp.HintedPlan)
	}
}
