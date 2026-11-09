package tuner

import "testing"

// tuning-02: the apply INSERT relies on pg_hint_plan 1.7's unique index on
// (query_id, application_name) to fail loudly over an existing row, so a
// hint table without that index is not ready and hints stay advisory.
//
// No concurrent access tests: checkHintTable is a single read-only catalog
// probe.
func TestCheckHintTable_RequiresUniqueIndex(t *testing.T) {
	pool, ctx := noHintTableDB(t)
	if checkHintTable(ctx, pool) {
		t.Fatal("no hint table reported ready")
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA hint_plan"); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name, sql string
		ready     bool
	}{
		{"table without an index", `CREATE TABLE hint_plan.hints (id serial PRIMARY KEY,
				query_id bigint NOT NULL, application_name text NOT NULL,
				hints text NOT NULL)`, false},
		{"non-unique index", `CREATE INDEX hints_plain
			ON hint_plan.hints (query_id, application_name)`, false},
		{"partial unique index", `CREATE UNIQUE INDEX hints_partial
			ON hint_plan.hints (query_id, application_name) WHERE hints <> ''`, false},
		{"unique index over another column", `CREATE UNIQUE INDEX hints_text
			ON hint_plan.hints (query_id, hints)`, false},
		{"pg_hint_plan 1.7 unique index", `CREATE UNIQUE INDEX hints_id_and_app
			ON hint_plan.hints (query_id, application_name)`, true},
	}
	for _, s := range steps {
		if _, err := pool.Exec(ctx, s.sql); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if got := checkHintTable(ctx, pool); got != s.ready {
			t.Fatalf("%s: checkHintTable = %t, want %t", s.name, got, s.ready)
		}
	}
}
