package tuner

import (
	"context"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func prepareHintTables(t *testing.T, pool *pgxpool.Pool, ids []int64) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE SCHEMA IF NOT EXISTS hint_plan`,
		`CREATE TABLE IF NOT EXISTS hint_plan.hints (
			id serial PRIMARY KEY, query_id bigint NOT NULL,
			application_name text NOT NULL, hints text NOT NULL)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("prepare hint_plan: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM hint_plan.hints WHERE query_id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM sage.query_hints WHERE queryid = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
}

// C11: a retired or broken sage hint whose hint_plan.hints row is still
// installed yields a removal finding that the executor applies through
// the retire_query_hint path. Active hints and hints pg_sage never owned
// are left alone.
func TestHintRemovalFindings_RetiredAndBrokenOnly(t *testing.T) {
	pool := connectTunerTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	ids := []int64{9001, 9002, 9003, 9004}
	prepareHintTables(t, pool, ids)
	for _, row := range []struct {
		id     int64
		status string
	}{{9001, "retired"}, {9002, "broken"}, {9003, "retired"}, {9003, "active"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
			(queryid, hint_text, symptom, status) VALUES ($1, 'HashJoin(a b)', 's', $2)`,
			row.id, row.status); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		if _, err := pool.Exec(ctx, `INSERT INTO hint_plan.hints
			(query_id, application_name, hints) VALUES ($1, '', 'HashJoin(a b)')`, id); err != nil {
			t.Fatal(err)
		}
	}
	ready := &HintPlanAvailability{Available: true, HintTableReady: true}
	tu := New(pool, TunerConfig{}, ready, noopLog2)
	findings := tu.hintRemovalFindings(ctx)
	var got []string
	for _, f := range findings {
		got = append(got, f.ObjectIdentifier)
		if f.Category != "query_hint_retirement" || f.ActionRisk != "safe" {
			t.Errorf("finding %s: category=%q risk=%q", f.ObjectIdentifier, f.Category, f.ActionRisk)
		}
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "queryid:9001" || got[1] != "queryid:9002" {
		t.Fatalf("removal findings = %v, want [queryid:9001 queryid:9002]", got)
	}
	for _, f := range findings {
		want := BuildDeleteSQL(9001)
		if f.ObjectIdentifier == "queryid:9002" {
			want = BuildDeleteSQL(9002)
		}
		if f.RecommendedSQL != want {
			t.Errorf("%s SQL = %q, want %q", f.ObjectIdentifier, f.RecommendedSQL, want)
		}
	}
	if none := New(pool, TunerConfig{}, nil, noopLog2).hintRemovalFindings(ctx); len(none) != 0 {
		t.Errorf("findings without pg_hint_plan = %d, want 0", len(none))
	}
}
