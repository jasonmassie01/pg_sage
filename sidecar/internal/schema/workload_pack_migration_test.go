package schema

import (
	"strings"
	"testing"
)

// evid-02a: sage.workload_packs stores each built workload evidence pack
// once per database and content hash. The step follows the evid-01b rollup
// tables in tranche five. No concurrent access tests: the step is a constant string (the
// bootstrap lock serializes it; see the DB test).

func TestWorkloadPacksStep_IsATrancheFiveStep(t *testing.T) {
	steps := trancheFiveMigrations()
	idx := -1
	for i, step := range steps {
		if step[0] == "ddlWorkloadPacks" && step[1] == ddlWorkloadPacks {
			idx = i
		}
	}
	// evid-03a's ddlFactsOwnerBrief merged in after this step; the pack
	// step must stay in tranche five, after the evid-01b rollup tables.
	if idx < 1 || steps[idx-1][0] != "ddlHistoryMigrationRollupTables" {
		t.Fatalf("ddlWorkloadPacks at %d of tranche five, want after the rollup tables", idx)
	}
	var found []string
	for _, step := range migrationSteps() {
		if strings.Contains(step.sql, "CREATE TABLE IF NOT EXISTS sage.workload_packs") {
			found = append(found, step.name)
		}
	}
	if len(found) != 1 || found[0] != "ddlWorkloadPacks" {
		t.Fatalf("steps creating sage.workload_packs = %v, want only ddlWorkloadPacks", found)
	}
}

func TestWorkloadPacksStep_IsIdempotentBoundedAndChecked(t *testing.T) {
	for _, want := range []string{
		"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.workload_packs",
		"CHECK (tier = 'local')",
		"CREATE UNIQUE INDEX IF NOT EXISTS workload_packs_key",
		"(COALESCE(database_id, 0), content_sha256)",
		"CREATE INDEX IF NOT EXISTS workload_packs_age",
		"body              bytea NOT NULL",
		"CHECK (bytes >= 0 AND bytes <= 33554432)",
	} {
		if !strings.Contains(ddlWorkloadPacks, want) {
			t.Errorf("ddlWorkloadPacks lacks %q", want)
		}
	}
	for _, bad := range []string{"DROP ", "UPDATE ", "ALTER TABLE"} {
		if strings.Contains(ddlWorkloadPacks, bad) {
			t.Errorf("ddlWorkloadPacks contains %q", bad)
		}
	}
}
