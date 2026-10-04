package tuning

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/verify"
)

// The agent's reads on real PostgreSQL: outcomes for calibration,
// operator rejections, open findings, plans (cache and generic), and the
// statistics catalog.

func pgStore(t *testing.T, pool *pgxpool.Pool) Store {
	t.Helper()
	return NewPostgresStore(pool, serverVersion(t, pool), catalogread.Default())
}

func seedOutcome(t *testing.T, pool *pgxpool.Pool, class, method string, expected float64,
	verdict string, decided time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('create_index_concurrently', 'CREATE INDEX CONCURRENTLY x ON t (a)')
		RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	store := verify.NewOutcomeStore(pool)
	p := verify.Prediction{Class: class, Method: method, Metric: verify.MetricMeanExecTime,
		ExpectedChangePct: pct(expected), Source: PredictionSource}
	if err := store.RecordPrediction(ctx, id, p); err != nil {
		t.Fatalf("record prediction: %v", err)
	}
	if verdict == "pending" {
		return id
	}
	observed := -25.0
	if err := store.RecordVerdict(ctx, verify.Outcome{ActionLogID: id, Class: class,
		Verdict: verdict, Observed: verify.Observed{Metric: verify.MetricMeanExecTime,
			Before: 10, After: 7.5, ChangePct: &observed}}); err != nil {
		t.Fatalf("record verdict: %v", err)
	}
	mustExec(t, pool, `UPDATE sage.action_outcome SET decided_at = $2
		WHERE action_log_id = $1`, id, decided)
	return id
}

func TestPostgresStore_Outcomes(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	mustExec(t, pool, "DELETE FROM sage.action_outcome WHERE action_class IN "+
		"('index_create','guc')")
	now := time.Now()
	seedOutcome(t, pool, "index_create", "hypopg", -40, "improved", now.Add(-time.Hour))
	seedOutcome(t, pool, "index_create", "hypopg", -30, "neutral", now.Add(-2*time.Hour))
	seedOutcome(t, pool, "index_create", "model", -20, "regressed", now.Add(-400*24*time.Hour))
	seedOutcome(t, pool, "index_create", "hypopg", -40, "pending", now)
	seedOutcome(t, pool, "guc", "model", -50, "improved", now.Add(-time.Hour))
	store := pgStore(t, pool)
	got, err := store.Outcomes(ctx, []string{"index_create"}, now.Add(-180*24*time.Hour), 100)
	if err != nil {
		t.Fatalf("outcomes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("outcomes = %+v: decided, in the window, of the class only", got)
	}
	first := got[0]
	if first.Class != "index_create" || first.Method != "hypopg" || first.PredictedPct != -40 ||
		first.Verdict != "improved" || first.Tolerance != verify.ToleranceMet ||
		first.ObservedPct == nil || *first.ObservedPct != -25 {
		t.Fatalf("newest first, every field read back: %+v", first)
	}
	limited, err := store.Outcomes(ctx, []string{"index_create", "guc"},
		now.Add(-180*24*time.Hour), 1)
	if err != nil || len(limited) != 2 {
		t.Fatalf("the limit applies per class: %+v %v", limited, err)
	}
	none, err := store.Outcomes(ctx, nil, now.Add(-time.Hour), 10)
	if err != nil || len(none) != 0 {
		t.Fatalf("no classes, no read: %+v %v", none, err)
	}
}

func TestPostgresStore_OperatorRejected(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	s := freshSchema(t, pool)
	q := `INSERT INTO sage.action_queue (proposed_sql, action_risk, status, decided_by,
		decided_at, proposed_at) VALUES ($1, 'moderate', $2, $3, now(), $4)`
	now := time.Now()
	mustExec(t, pool, q, "DROP INDEX CONCURRENTLY IF EXISTS "+s+".a_idx;", "rejected", 1, now)
	mustExec(t, pool, q, "DROP INDEX CONCURRENTLY IF EXISTS "+s+".b_idx", "rejected", nil, now)
	mustExec(t, pool, q, "DROP INDEX CONCURRENTLY IF EXISTS "+s+".c_idx", "pending", nil, now)
	mustExec(t, pool, q, "DROP INDEX CONCURRENTLY IF EXISTS "+s+".d_idx", "rejected", 1,
		now.Add(-30*24*time.Hour))
	got, err := pgStore(t, pool).OperatorRejected(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("operator rejected: %v", err)
	}
	if !got[normalizeSQL("drop index concurrently if exists "+s+".a_idx")] {
		t.Fatalf("an operator's rejection is remembered (normalized): %v", got)
	}
	for _, name := range []string{"b_idx", "c_idx", "d_idx"} {
		if got[normalizeSQL("DROP INDEX CONCURRENTLY IF EXISTS "+s+"."+name)] {
			t.Fatalf("%s: superseded, pending or old items are not rejections", name)
		}
	}
}

func seedFinding(t *testing.T, pool *pgxpool.Pool, category, ident, status string,
	detail map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(detail)
	mustExec(t, pool, `INSERT INTO sage.findings (category, severity, object_type,
		object_identifier, title, detail, recommendation, recommended_sql, rollback_sql,
		status) VALUES ($1, 'info', 'index', $2, 'title', $3, 'why', 'CREATE INDEX x',
		'DROP INDEX x', $4)`, category, ident, raw, status)
}

func TestPostgresStore_OpenFindings(t *testing.T) {
	pool := dbPool(t)
	s := freshSchema(t, pool)
	seedFinding(t, pool, "missing_index", s+".orders|a", "open",
		map[string]any{"producer": Producer, "case_id": "top_statement:1", "table": s + ".orders"})
	seedFinding(t, pool, "missing_index", s+".orders|b", "open",
		map[string]any{"table": s + ".orders", "llm_rationale": "legacy"})
	seedFinding(t, pool, "missing_index", s+".orders|c", "resolved",
		map[string]any{"producer": Producer, "case_id": "top_statement:1"})
	seedFinding(t, pool, "slow_query", s+".orders|d", "open", map[string]any{})
	got, err := pgStore(t, pool).OpenFindings(context.Background(),
		[]string{"missing_index", "memory_tuning"})
	if err != nil {
		t.Fatalf("open findings: %v", err)
	}
	var idents []string
	for _, f := range got {
		if strings.HasPrefix(f.ObjectIdentifier, s+".") {
			idents = append(idents, f.ObjectIdentifier)
		}
	}
	slices.Sort(idents)
	if !slices.Equal(idents, []string{s + ".orders|a", s + ".orders|b"}) {
		t.Fatalf("open findings = %v", idents)
	}
	for _, f := range got {
		if f.ObjectIdentifier == s+".orders|a" && (f.Detail["case_id"] != "top_statement:1" ||
			f.RecommendedSQL != "CREATE INDEX x" || f.RollbackSQL != "DROP INDEX x" ||
			f.Recommendation != "why") {
			t.Fatalf("finding read back = %+v", f)
		}
	}
}

func TestPostgresStore_PlanFromTheCacheThenGeneric(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	s := freshSchema(t, pool)
	mustExec(t, pool, "CREATE TABLE "+s+".t (id bigint PRIMARY KEY, v int)")
	mustExec(t, pool, "INSERT INTO "+s+".t SELECT g, g FROM generate_series(1, 1000) g")
	mustExec(t, pool, `INSERT INTO sage.explain_cache (queryid, query_text, plan_json, source)
		VALUES (7001, 'q', '[{"Plan":{"Node Type":"Index Scan"}}]', 'auto_explain')`)
	store := pgStore(t, pool)
	p, err := store.Plan(ctx, 7001, "SELECT * FROM "+s+".t WHERE id = $1")
	if err != nil || p.Source != PlanSourceCache ||
		!strings.Contains(string(p.JSON), "Index Scan") {
		t.Fatalf("cached plan = %+v %v", p, err)
	}
	p, err = store.Plan(ctx, 7002, "SELECT * FROM "+s+".t WHERE v = $1")
	if serverVersion(t, pool) < 160000 {
		if err != nil || p.Source != PlanSourceNone {
			t.Fatalf("before PG16 a parameterized statement has no plan: %+v %v", p, err)
		}
		return
	}
	if err != nil || p.Source != PlanSourceGeneric || !strings.Contains(string(p.JSON),
		"Seq Scan") {
		t.Fatalf("generic plan = %+v %v", p, err)
	}
}

func TestPostgresStore_PlanNeverExecutes(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	s := freshSchema(t, pool)
	mustExec(t, pool, "CREATE TABLE "+s+".t (id bigint PRIMARY KEY, v int)")
	mustExec(t, pool, "INSERT INTO "+s+".t VALUES (1, 1)")
	mustExec(t, pool, "CREATE SEQUENCE "+s+".seq")
	store := pgStore(t, pool)
	for _, q := range []string{
		"UPDATE " + s + ".t SET v = 99 WHERE id = 1",
		"SELECT nextval('" + s + ".seq')",
		"DELETE FROM " + s + ".t",
	} {
		if _, err := store.Plan(ctx, 7100, q); err != nil {
			t.Fatalf("plan %q: %v", q, err)
		}
	}
	var v int
	var seq int64
	if err := pool.QueryRow(ctx, "SELECT v FROM "+s+".t WHERE id = 1").Scan(&v); err != nil ||
		v != 1 {
		t.Fatalf("EXPLAIN without ANALYZE never runs the statement: v = %d %v", v, err)
	}
	if err := pool.QueryRow(ctx, "SELECT last_value FROM "+s+".seq").Scan(&seq); err != nil ||
		seq != 1 {
		t.Fatalf("the sequence did not advance: %d %v", seq, err)
	}
	if _, err := store.Plan(ctx, 7101, "SELECT 1; DROP TABLE "+s+".t"); err == nil {
		t.Fatal("multiple statements are refused")
	}
	var exists bool
	_ = pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", s+".t").Scan(&exists)
	if !exists {
		t.Fatal("the table must survive")
	}
}

func TestPostgresStore_StatisticsCatalog(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	s := freshSchema(t, pool)
	mustExec(t, pool, "CREATE TABLE "+s+".t (a int, b int, c text)")
	mustExec(t, pool, "INSERT INTO "+s+".t SELECT g % 10, g % 10, 'x' "+
		"FROM generate_series(1, 2000) g")
	mustExec(t, pool, "CREATE STATISTICS "+s+".t_ab (dependencies) ON a, b FROM "+s+".t")
	mustExec(t, pool, "ANALYZE "+s+".t")
	store := pgStore(t, pool)
	ext, err := store.ExtendedStats(ctx, s, "t")
	if err != nil || len(ext) != 1 || ext[0].Name != "t_ab" ||
		!slices.Equal(ext[0].Columns, []string{"a", "b"}) ||
		!slices.Equal(ext[0].Kinds, []string{"dependencies"}) {
		t.Fatalf("extended stats = %+v %v", ext, err)
	}
	cols, err := store.ColumnStats(ctx, s, "t", []string{"a", "c", "missing"})
	if err != nil || len(cols) != 2 {
		t.Fatalf("column stats = %+v %v", cols, err)
	}
	if cols[0].Column != "a" || cols[0].NDistinct != 10 || cols[0].AvgWidth != 4 {
		t.Fatalf("a = %+v", cols[0])
	}
	if none, err := store.ExtendedStats(ctx, s, "nope"); err != nil || len(none) != 0 {
		t.Fatalf("unknown table: %+v %v", none, err)
	}
}
