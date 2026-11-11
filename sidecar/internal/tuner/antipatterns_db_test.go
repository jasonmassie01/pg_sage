package tuner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// market-10 on real PostgreSQL (14 and 17 here; 18 when the test server is
// 18): EXPLAIN (ANALYZE, FORMAT JSON) of a statement whose index on (a)
// leaves a filter on b yields index_filter_inefficient, and an OR join
// yields or_join_filter. The plan JSON is the server's own, so a change in
// how a version prints the counters (PG18 decimals) is caught here.

// antipatternSchema creates a schema with ip (index on a only) and the OR
// join pair oa / ob, analyzed; cleanup drops it.
func antipatternSchema(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	s := fmt.Sprintf("tap_%d", time.Now().UnixNano()%1_000_000_000)
	execAll(t, pool, "CREATE SCHEMA "+s,
		"CREATE TABLE "+s+".ip (id int, a int, b int, pad text)",
		"INSERT INTO "+s+".ip SELECT g, g % 16, g % 97, 'x' FROM generate_series(1, 200000) g",
		"CREATE INDEX ip_a_idx ON "+s+".ip (a)",
		"CREATE TABLE "+s+".oa (id int, x int, y int)",
		"CREATE TABLE "+s+".ob (id int, x int, y int)",
		"INSERT INTO "+s+".oa SELECT g, g % 50, g % 7 FROM generate_series(1, 200) g",
		"INSERT INTO "+s+".ob SELECT g, g % 60, g % 11 + 100 FROM generate_series(1, 200) g",
		"ANALYZE "+s+".ip", "ANALYZE "+s+".oa", "ANALYZE "+s+".ob")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+s+" CASCADE")
	})
	return s
}

// explainAnalyze runs EXPLAIN (ANALYZE, FORMAT JSON) with seq scans off so
// the planner reads ip through its index whatever the version's costs.
func explainAnalyze(t *testing.T, pool *pgxpool.Pool, sql string) []byte {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql).Scan(&raw); err != nil {
		t.Fatalf("explain %s: %v", sql, err)
	}
	return raw
}

func indexFilterQuery(s string) string {
	return "SELECT id FROM " + s + ".ip WHERE a = 5 AND b = 7"
}

func TestAntipatternDB_IndexFilterInefficient(t *testing.T) {
	pool, _ := requireTunerDB(t)
	s := antipatternSchema(t, pool)
	plan := explainAnalyze(t, pool, indexFilterQuery(s))
	got := scanKind(t, plan, SymptomIndexFilterInefficient)
	if len(got) != 1 {
		t.Fatalf("index_filter_inefficient = %d, want 1; plan %s", len(got), plan)
	}
	d := got[0].Detail
	removed, _ := d["rows_removed"].(int64)
	kept, _ := d["rows_kept"].(int64)
	// a = 5 matches 12,500 rows; b = 7 keeps those with g % 97 = 7 too.
	if got[0].IndexName != "ip_a_idx" || got[0].RelationName != "ip" ||
		removed+kept != 12_500 || kept < 100 || kept > 160 {
		t.Fatalf("symptom = %+v; plan %s", got[0], plan)
	}
	cols, _ := d["filter_columns"].([]string)
	if len(cols) != 1 || cols[0] != "b" {
		t.Fatalf("filter_columns = %#v", d["filter_columns"])
	}
	p := Prescribe(got[0], TunerConfig{})
	if p.HintDirective != "" || !containsAll(p.Rationale, "ip_a_idx", "(a, b)") {
		t.Fatalf("prescription = %+v", p)
	}
}

func TestAntipatternDB_ORJoinFilter(t *testing.T) {
	pool, _ := requireTunerDB(t)
	s := antipatternSchema(t, pool)
	plan := explainAnalyze(t, pool, "SELECT count(*) FROM "+s+".oa a JOIN "+s+
		".ob b ON (a.x = b.x OR a.y = b.y)")
	got := scanKind(t, plan, SymptomORJoinFilter)
	if len(got) != 1 {
		t.Fatalf("or_join_filter = %d, want 1; plan %s", len(got), plan)
	}
	arms, _ := got[0].Detail["arms"].([]string)
	removed, _ := got[0].Detail["rows_removed"].(int64)
	// 200 x 200 pairs; a.y < 7 never meets b.y >= 100, so only x matches.
	if len(arms) != 2 || removed < 39_000 || removed >= 40_000 {
		t.Fatalf("symptom = %+v; plan %s", got[0], plan)
	}
}

// State: the advisory finding writes no sage.query_hints row and carries no
// SQL even with pg_hint_plan usable (where the server's hint table has the
// 1.7 query_id layout; PG14-16 builds have the older one and stay advisory).
func TestAntipatternDB_FindingWritesNoHint(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	s := antipatternSchema(t, pool)
	qid := -time.Now().UnixNano()
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.explain_cache WHERE queryid = $1", qid)
		_, _ = pool.Exec(bg, "DELETE FROM sage.query_hints WHERE queryid = $1", qid)
	})
	cachePlan(t, pool, qid, "0 seconds", string(explainAnalyze(t, pool, indexFilterQuery(s))))
	ready := &HintPlanAvailability{ApplicationEffect: true,
		HintTableReady: checkHintTable(ctx, pool)}
	tu := New(pool, TunerConfig{MinQueryCalls: 1}, ready, noopLogFn)
	findings, err := tu.tuneCandidates(ctx, []candidate{{QueryID: qid,
		Query: indexFilterQuery(s), Calls: 500, MeanExecTime: 500}}, nil)
	if err != nil {
		t.Fatalf("tuneCandidates: %v", err)
	}
	if len(findings) != 1 || findings[0].RecommendedSQL != "" ||
		findings[0].Detail["plan_antipatterns"] == nil {
		t.Fatalf("findings = %+v", findings)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage.query_hints WHERE queryid = $1",
		qid).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sage.query_hints rows = %d (%v), want 0", n, err)
	}
}

func containsAll(s string, frags ...string) bool {
	for _, f := range frags {
		if !strings.Contains(s, f) {
			return false
		}
	}
	return true
}
