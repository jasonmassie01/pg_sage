package schema

import (
	"context"
	"strings"
	"testing"
)

// drill-02a on a real server: bootstrap twice is clean and
// sage.upgrade_briefs keeps one row per stored brief, refusing a row
// without its hash or with a nonsense target. Delta-only storage and
// concurrent writers are upgradecheck.TestBriefDB_ConcurrentStoreKeepsOneRow.
func TestUpgradeBriefsMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "upgbriefs")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'upgrade_briefs'
		  AND column_name IN ('id', 'database', 'target_major', 'built_at', 'brief',
		      'hash')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 6 {
		t.Fatalf("sage.upgrade_briefs has %d of the 6 columns", cols)
	}
	ok := `INSERT INTO sage.upgrade_briefs (database, target_major, brief, hash)
		VALUES ('orders', 18, '{}', repeat('a', 64))`
	for range 2 { // a re-appearing brief is a new row: the store, not the table, dedupes
		if _, err := pool.Exec(ctx, ok); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for _, bad := range []string{
		`INSERT INTO sage.upgrade_briefs (database, target_major, brief, hash)
		 VALUES ('orders', 0, '{}', repeat('a', 64))`,
		`INSERT INTO sage.upgrade_briefs (database, target_major, brief, hash)
		 VALUES ('orders', 18, '{}', 'short')`,
		`INSERT INTO sage.upgrade_briefs (database, target_major, brief, hash)
		 VALUES ('', 18, '{}', repeat('a', 64))`,
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Fatalf("stored an invalid row: %s", bad)
		}
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'sage'
		AND indexname = 'upgrade_briefs_latest'`).Scan(&def); err != nil {
		t.Fatalf("latest index: %v", err)
	}
	if !strings.Contains(def, "database") || !strings.Contains(def, "target_major") {
		t.Fatalf("latest index = %s", def)
	}
}

func TestUpgradeBriefsStep_IsAT6StepIdempotentAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	// guard-02b's step follows it, so it is pinned after its predecessor.
	if i := stepIndex(steps, "ddlUpgradeBriefs"); i < 1 || steps[i-1][0] != "ddlReviewRuns" {
		t.Fatalf("T6 steps = %v, want ddlUpgradeBriefs after ddlReviewRuns", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.upgrade_briefs",
		"CREATE INDEX IF NOT EXISTS upgrade_briefs_latest"} {
		if !strings.Contains(ddlUpgradeBriefs, want) {
			t.Errorf("ddlUpgradeBriefs lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlUpgradeBriefs) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}
