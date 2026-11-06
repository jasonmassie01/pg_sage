package lint

import (
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// vacuum-04 moved the expected-pages estimate into the shared catalog
// helper, which sums the pg_stats column widths into a row footprint. The
// old per-rule copy used the average column width, so every healthy table
// with several columns read as bloated.
// No concurrency test: the rule is one read-only catalog query.

func TestIntegration_BloatedTableEstimateUsesRowWidth(t *testing.T) {
	pool, ctx := requireDB(t)
	schema := createSchema(t, pool, ctx)
	ddl := fmt.Sprintf(`
		CREATE TABLE %[1]s.healthy (id bigint, a int, b text, c timestamptz, d numeric)
			WITH (autovacuum_enabled = false);
		INSERT INTO %[1]s.healthy
			SELECT g, g %% 100, md5(g::text), now(), g * 1.5 FROM generate_series(1, 20000) g;
		CREATE TABLE %[1]s.bloated (id bigint, a int, b text, c timestamptz, d numeric)
			WITH (autovacuum_enabled = false);
		INSERT INTO %[1]s.bloated
			SELECT g, g %% 100, md5(g::text), now(), g * 1.5 FROM generate_series(1, 20000) g;
		DELETE FROM %[1]s.bloated WHERE id %% 10 < 7;
		CREATE TABLE %[1]s.never_analyzed (id bigint, payload text)
			WITH (autovacuum_enabled = false);
		INSERT INTO %[1]s.never_analyzed
			SELECT g, md5(g::text) FROM generate_series(1, 20000) g;
		DELETE FROM %[1]s.never_analyzed WHERE id %% 10 < 7`, schema)
	_, err := pool.Exec(ctx, ddl)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf("VACUUM %s.bloated", schema))
	require.NoError(t, err)
	analyzeTable(t, pool, ctx, schema, "healthy")
	analyzeTable(t, pool, ctx, schema, "bloated")

	findings, err := (&ruleBloatedTable{}).Check(ctx, pool, defaultOpts(schema))
	require.NoError(t, err)
	if found := findingForTable(findings, schema, "healthy"); found != nil {
		t.Fatalf("healthy five-column table flagged: %s", found.Description)
	}
	if found := findingForTable(findings, schema, "never_analyzed"); found != nil {
		t.Fatalf("never-analyzed table flagged without an estimate: %s", found.Description)
	}
	if found := findingForTable(findings, schema, "bloated"); found == nil {
		t.Fatal("table with 70% of its rows removed was not flagged")
	}
}
