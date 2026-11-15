package schema

import (
	"strings"
	"testing"
)

// evid-01a: sage.query_rollup_hourly and sage.query_rollup_coverage hold
// the hourly per-queryid rollup of the snapshot queries category. One
// step, the first of trancheFiveMigrations.
// No concurrent access tests: the step is a constant string.

func TestQueryRollupStep_IsTheOnlyStepCreatingTheTables(t *testing.T) {
	for _, table := range []string{"sage.query_rollup_hourly", "sage.query_rollup_coverage"} {
		var found []string
		for _, step := range migrationSteps() {
			if strings.Contains(step.sql, table) {
				found = append(found, step.name)
			}
		}
		if len(found) != 1 || found[0] != "ddlQueryRollup" {
			t.Fatalf("steps naming %s = %v, want only ddlQueryRollup", table, found)
		}
	}
	steps := trancheFiveMigrations()
	if len(steps) == 0 || steps[0][0] != "ddlQueryRollup" {
		t.Fatalf("trancheFiveMigrations = %v, want ddlQueryRollup first", steps)
	}
}

func TestQueryRollupStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{
		"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.query_rollup_hourly",
		"CREATE TABLE IF NOT EXISTS sage.query_rollup_coverage",
		"database_id       integer",
		"epoch_break       boolean NOT NULL",
		"calls >= 0",
		"exec_ms >= 0",
		"temp_blks_written >= 0",
		"wal_bytes IS NULL OR wal_bytes >= 0",
		"mod(extract(epoch FROM hour)::numeric, 3600) = 0",
		"epoch_source IN ('recorded', 'counters_only')",
		"CREATE UNIQUE INDEX IF NOT EXISTS query_rollup_hourly_key",
		"(COALESCE(database_id, 0), queryid, hour)",
		"CREATE INDEX IF NOT EXISTS query_rollup_hourly_hour",
		"CREATE UNIQUE INDEX IF NOT EXISTS query_rollup_coverage_key",
		"(COALESCE(database_id, 0), hour)",
	} {
		if !strings.Contains(ddlQueryRollup, want) {
			t.Errorf("ddlQueryRollup lacks %q:\n%s", want, ddlQueryRollup)
		}
	}
	for _, bad := range []string{"DROP ", "TRUNCATE", "DELETE "} {
		if strings.Contains(ddlQueryRollup, bad) {
			t.Errorf("ddlQueryRollup runs %q:\n%s", bad, ddlQueryRollup)
		}
	}
}
