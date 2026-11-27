package indexes

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/gen/indexes"))
}

// livePortfolio builds review02a.t (a, b, c) with (a), (a, b) and (a, b, c)
// and an exact duplicate pair on review02a.u (x), with HypoPG installed or
// not, and returns a pool and the pack facts of those indexes.
func livePortfolio(t *testing.T, hypopg bool) (*pgxpool.Pool, []workpack.IndexFact) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	ext := "DROP EXTENSION IF EXISTS hypopg"
	if hypopg {
		ext = "CREATE EXTENSION IF NOT EXISTS hypopg"
	}
	for _, sql := range []string{ext, "DROP SCHEMA IF EXISTS review02a CASCADE",
		"CREATE SCHEMA review02a",
		"CREATE TABLE review02a.t (a int, b int, c int, pad text)",
		"INSERT INTO review02a.t SELECT i % 5000, i % 97, i % 13, repeat('x', 40) " +
			"FROM generate_series(1, 30000) i",
		"CREATE INDEX t_a ON review02a.t (a)", "CREATE INDEX t_ab ON review02a.t (a, b)",
		"CREATE INDEX t_abc ON review02a.t (a, b, c)",
		"CREATE TABLE review02a.u (x int)", "CREATE INDEX u_x1 ON review02a.u (x)",
		"CREATE INDEX u_x2 ON review02a.u (x)", "VACUUM ANALYZE review02a.t",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			if hypopg && sql == ext {
				t.Skipf("HypoPG not installable on this server: %v", err)
			}
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return pool, liveFacts(t, ctx, pool)
}

// liveFacts reads the fixture's index definitions and sizes from the
// catalog, as workpack's indexes section states them.
func liveFacts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []workpack.IndexFact {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT c.relname, t.relname, pg_get_indexdef(c.oid),
		pg_relation_size(c.oid) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid
		WHERE c.relnamespace = 'review02a'::regnamespace ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	overlaps := map[string][2][]string{
		"t_a": {{}, {"review02a.t_ab", "review02a.t_abc"}}, "t_ab": {{}, {"review02a.t_abc"}},
		"t_abc": {{}, {}}, "u_x1": {{"review02a.u_x2"}, {}}, "u_x2": {{"review02a.u_x1"}, {}},
	}
	var out []workpack.IndexFact
	for rows.Next() {
		var f workpack.IndexFact
		if err := rows.Scan(&f.Index, &f.Table, &f.Definition, &f.SizeBytes); err != nil {
			t.Fatal(err)
		}
		f.ID, f.Schema, f.Method, f.Valid = "index/idx/review02a."+f.Index, "review02a",
			"btree", true
		f.Scans, f.TableWrites = int64(3), workpack.Writes{Inserts: 9000}
		f.DuplicateOf, f.PrefixOf = overlaps[f.Index][0], overlaps[f.Index][1]
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func livePack(t *testing.T, facts []workpack.IndexFact) *workpack.Pack {
	t.Helper()
	s := spec("db1", 90, "ok", facts...)
	s.Workload.Families[0].Relations = []string{"review02a.t"}
	s.Texts = &workpack.Texts{Texts: []workpack.QueryText{
		{QueryID: "7", Text: "SELECT a FROM review02a.t WHERE a = 42"},
		{QueryID: "9", Text: "SELECT a, b FROM review02a.t WHERE a = 42 AND b = 3"}}}
	return mustPack(t, s)
}

func TestIndexesDB_HypoPGKeepsTheConsolidationSetAsSteps(t *testing.T) {
	pool, facts := livePortfolio(t, true)
	pack := livePack(t, facts)
	cands, _, err := New(liveMeasurer(pool)).Generate(t.Context(), pack, env("db1"))
	if err != nil {
		t.Fatal(err)
	}
	got := byTarget(cands)
	for _, name := range []string{"review02a.t_a", "review02a.t_ab"} {
		c := got[name]
		if c.ActionType != "drop_unused_index" || c.Prediction.Tier != "plan_only" {
			t.Errorf("%s: %s tier %q reason %q", name, c.ActionType, c.Prediction.Tier,
				c.Reason)
		}
	}
	if dup := got["review02a.u_x2"]; dup.ActionType != "drop_unused_index" {
		t.Errorf("duplicate step %+v", dup)
	}
	assertNoneHidden(t, pool)
}

// CHECK-11 on a server without HypoPG: the set is advice with tier none,
// the reason says hypopg, and the plain duplicate is still a step.
func TestIndexesDB_WithoutHypoPGTheSetIsAdvice(t *testing.T) {
	pool, facts := livePortfolio(t, false)
	pack := livePack(t, facts)
	cands, _, err := New(liveMeasurer(pool)).Generate(t.Context(), pack, env("db1"))
	if err != nil {
		t.Fatal(err)
	}
	got := byTarget(cands)
	for _, name := range []string{"review02a.t_a", "review02a.t_ab"} {
		c := got[name]
		if c.ActionType != candidate.ActionAdvice || c.Prediction.Tier != candidate.TierNone ||
			!strings.Contains(c.Reason, "hypopg") {
			t.Errorf("%s: %s tier %q reason %q", name, c.ActionType, c.Prediction.Tier,
				c.Reason)
		}
	}
	if dup := got["review02a.u_x2"]; dup.ActionType != "drop_unused_index" ||
		dup.Validate(pack) != nil {
		t.Errorf("duplicate step %+v", dup)
	}
}

func liveMeasurer(pool *pgxpool.Pool) Measurer {
	return NewHypoPGMeasurer(pool, func(level, format string, args ...any) {})
}

func assertNoneHidden(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var hidden int
	if err := conn.QueryRow(t.Context(),
		"SELECT count(*) FROM hypopg_hidden_indexes()").Scan(&hidden); err != nil {
		t.Fatalf("read hidden indexes: %v", err)
	}
	if hidden != 0 {
		t.Fatalf("%d indexes still hidden on a pooled connection", hidden)
	}
}
