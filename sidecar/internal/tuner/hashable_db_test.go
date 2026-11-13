package tuner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// market-10b on real PostgreSQL: the server's own EXPLAIN (ANALYZE, FORMAT
// JSON) of a misestimated nested loop joined only by OR gives no HashJoin,
// while the same join on an equality (in the Join Filter or as a
// parameterized index scan) does. x and y are equal, so x = 1 AND y = 1 is
// estimated at about one row of 2,000 but returns 50 (the misestimate);
// pg_hint_plan is not needed.

// hashableSchema creates oa (2,000 rows) and ob (1,000 rows, a_id odd) in a
// fresh schema, analyzed; cleanup drops it.
func hashableSchema(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	s := fmt.Sprintf("thj_%d", time.Now().UnixNano()%1_000_000_000)
	execAll(t, pool, "CREATE SCHEMA "+s,
		"CREATE TABLE "+s+".oa (id int, x int, y int)",
		"CREATE TABLE "+s+".ob (id int, a_id int, x int, y int)",
		"INSERT INTO "+s+".oa SELECT g, g % 40, g % 40 FROM generate_series(1, 2000) g",
		"INSERT INTO "+s+".ob SELECT g, g * 2 - 1, g % 40, g % 40 "+
			"FROM generate_series(1, 1000) g",
		"ANALYZE "+s+".oa", "ANALYZE "+s+".ob")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+s+" CASCADE")
	})
	return s
}

// explainNestedLoop runs EXPLAIN (ANALYZE, FORMAT JSON) with hash and merge
// joins off, so the equality join is planned as a nested loop too.
func explainNestedLoop(t *testing.T, pool *pgxpool.Pool, sql string) []byte {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, set := range []string{"SET LOCAL enable_hashjoin = off",
		"SET LOCAL enable_mergejoin = off", "SET LOCAL statement_timeout = '30s'"} {
		if _, err := tx.Exec(ctx, set); err != nil {
			t.Fatalf("%s: %v", set, err)
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql).Scan(&raw); err != nil {
		t.Fatalf("explain %s: %v", sql, err)
	}
	return raw
}

func hashableJoinQuery(s, on string) string {
	return "SELECT count(*) FROM " + s + ".oa a JOIN " + s + ".ob b ON " + on +
		" WHERE a.x = 1 AND a.y = 1"
}

// dbBadNestedLoop returns the plan's single bad_nested_loop, failing with
// the plan when there is none (the positive control of the fixture).
func dbBadNestedLoop(t *testing.T, plan []byte, opts ...ScanOption) PlanSymptom {
	t.Helper()
	got := scanKind(t, plan, SymptomBadNestedLoop, opts...)
	if len(got) != 1 {
		t.Fatalf("bad_nested_loop = %d, want 1; plan %s", len(got), plan)
	}
	return got[0]
}

func TestHashableDB_ORJoinGetsNoHashJoin(t *testing.T) {
	pool, _ := requireTunerDB(t)
	s := hashableSchema(t, pool)
	plan := explainNestedLoop(t, pool, hashableJoinQuery(s, "(a.x = b.x OR a.y = b.y)"))
	bnl := dbBadNestedLoop(t, plan)
	if h, ok := bnl.Detail["hashable_join"].(bool); !ok || h {
		t.Fatalf("hashable_join = %#v, want false; plan %s", bnl.Detail["hashable_join"], plan)
	}
	if p := Prescribe(bnl, TunerConfig{}); p != nil {
		t.Fatalf("OR join prescribed %+v; plan %s", p, plan)
	}
	if got := scanKind(t, plan, SymptomORJoinFilter); len(got) != 1 {
		t.Fatalf("or_join_filter = %d, want 1; plan %s", len(got), plan)
	}
}

func TestHashableDB_EqualityJoinGetsHashJoin(t *testing.T) {
	pool, _ := requireTunerDB(t)
	s := hashableSchema(t, pool)
	plan := explainNestedLoop(t, pool, hashableJoinQuery(s, "a.id = b.a_id"))
	bnl := dbBadNestedLoop(t, plan)
	if h, _ := bnl.Detail["hashable_join"].(bool); !h {
		t.Fatalf("hashable_join = %#v, want true; plan %s", bnl.Detail["hashable_join"], plan)
	}
	p := Prescribe(bnl, TunerConfig{})
	if p == nil || p.HintDirective != "HashJoin(a b)" {
		t.Fatalf("prescription = %+v, want HashJoin(a b); plan %s", p, plan)
	}
}

// With an index on ob.a_id the equality moves into the inner index scan's
// Index Cond (a parameterized scan); the join is still hashable. The inner
// side reads one row per loop, so the row threshold is lowered to 1.
func TestHashableDB_ParameterizedIndexScanIsHashable(t *testing.T) {
	pool, _ := requireTunerDB(t)
	s := hashableSchema(t, pool)
	execAll(t, pool, "CREATE INDEX ob_a_id_idx ON "+s+".ob (a_id)", "ANALYZE "+s+".ob")
	plan := explainNestedLoop(t, pool, hashableJoinQuery(s, "a.id = b.a_id"))
	bnl := dbBadNestedLoop(t, plan, WithNestedLoopMinRows(1))
	if h, _ := bnl.Detail["hashable_join"].(bool); !h {
		t.Fatalf("hashable_join = %#v, want true; plan %s", bnl.Detail["hashable_join"], plan)
	}
	if p := Prescribe(bnl, TunerConfig{}); p == nil || p.HintDirective != "HashJoin(a b)" {
		t.Fatalf("prescription = %+v, want HashJoin(a b); plan %s", p, plan)
	}
}

// With indexes on ob.x and ob.y the OR join becomes a parameterized
// Bitmap Heap Scan over a BitmapOr: each arm's Index Cond is an equality
// on the outer input, but the join as a whole is still a pure OR (the
// Recheck Cond), so it cannot be hash-joined. The inner side reads 25 rows
// per loop, so the row threshold is lowered to 1.
func TestHashableDB_IndexedORJoinGetsNoHashJoin(t *testing.T) {
	pool, _ := requireTunerDB(t)
	s := hashableSchema(t, pool)
	execAll(t, pool, "CREATE INDEX ob_x_idx ON "+s+".ob (x)",
		"CREATE INDEX ob_y_idx ON "+s+".ob (y)", "ANALYZE "+s+".ob")
	plan := explainNestedLoop(t, pool, hashableJoinQuery(s, "(a.x = b.x OR a.y = b.y)"))
	if !strings.Contains(string(plan), `"BitmapOr"`) {
		t.Fatalf("fixture: want a BitmapOr inner side; plan %s", plan)
	}
	bnl := dbBadNestedLoop(t, plan, WithNestedLoopMinRows(1))
	if h, ok := bnl.Detail["hashable_join"].(bool); !ok || h {
		t.Fatalf("hashable_join = %#v, want false; plan %s", bnl.Detail["hashable_join"], plan)
	}
	if p := Prescribe(bnl, TunerConfig{}); p != nil {
		t.Fatalf("indexed OR join prescribed %+v; plan %s", p, plan)
	}
}
