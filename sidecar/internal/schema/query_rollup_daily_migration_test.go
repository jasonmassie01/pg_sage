package schema

import (
	"strings"
	"testing"
)

// evid-01b: sage.query_rollup_daily holds the daily tier of the per-query
// rollup, and sage.history_migration's table_name check admits the derived
// rollup tables a history migration copies. Two steps, after ddlQueryRollup.
// No concurrent access tests: the steps are constant strings.

func TestQueryRollupDailySteps_FollowTheHourlyStep(t *testing.T) {
	steps := trancheFiveMigrations()
	at := map[string]int{}
	for i, s := range steps {
		at[s[0]] = i
	}
	h, okH := at["ddlQueryRollup"]
	d, okD := at["ddlQueryRollupDaily"]
	m, okM := at["ddlHistoryMigrationRollupTables"]
	if !okH || !okD || !okM || d != h+1 || m != d+1 {
		t.Fatalf("trancheFiveMigrations = %v, want ddlQueryRollup, ddlQueryRollupDaily, "+
			"ddlHistoryMigrationRollupTables in a row", steps)
	}
	var found []string
	for _, step := range migrationSteps() {
		if strings.Contains(step.sql, "sage.query_rollup_daily") {
			found = append(found, step.name)
		}
	}
	if len(found) != 1 || found[0] != "ddlQueryRollupDaily" {
		t.Fatalf("steps naming sage.query_rollup_daily = %v, want only ddlQueryRollupDaily",
			found)
	}
}

func TestQueryRollupDailyStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{
		"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.query_rollup_daily",
		"day               date NOT NULL",
		"calls >= 0", "exec_ms >= 0", "temp_blks_written >= 0",
		"wal_bytes IS NULL OR wal_bytes >= 0",
		"hours             smallint NOT NULL",
		"epoch_breaks      smallint NOT NULL",
		"plan_hashes       smallint NOT NULL",
		"last_plan_hash    text",
		"CREATE UNIQUE INDEX IF NOT EXISTS query_rollup_daily_key",
		"(COALESCE(database_id, 0), queryid, day)",
		"CREATE INDEX IF NOT EXISTS query_rollup_daily_age",
	} {
		if !strings.Contains(ddlQueryRollupDaily, want) {
			t.Errorf("ddlQueryRollupDaily lacks %q:\n%s", want, ddlQueryRollupDaily)
		}
	}
	for _, bad := range []string{"DROP ", "TRUNCATE", "DELETE "} {
		if strings.Contains(ddlQueryRollupDaily, bad) {
			t.Errorf("ddlQueryRollupDaily runs %q", bad)
		}
	}
	for _, want := range []string{"SET LOCAL lock_timeout", "pg_constraint",
		"to_regclass('sage.history_migration')", "'query_rollup_daily'",
		"'query_rollup_coverage'", "'snapshots'", "'query_store'"} {
		if !strings.Contains(ddlHistoryMigrationRollupTables, want) {
			t.Errorf("ddlHistoryMigrationRollupTables lacks %q", want)
		}
	}
	if !strings.Contains(ddlHistoryMigration, "'query_rollup_daily'") {
		t.Error("a progress table created from now on must admit the derived tables")
	}
}
