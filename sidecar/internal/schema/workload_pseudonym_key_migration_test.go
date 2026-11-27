package schema

import (
	"context"
	"strings"
	"testing"
)

// evid-02c: sage.workload_pseudonym_key holds the one sealed pseudonym key
// of the install. No concurrent access tests here: two creators are
// covered by workpack.TestPseudonymKeyDB_ConcurrentCallersAgree.

// The step follows ddlPlanApprovalModes; ux-01b's ddlUserInboxSeenAt
// follows it, so its place is pinned rather than "last".
func TestWorkloadPseudonymKeyStep_IsInPlaceBoundedAndIdempotent(t *testing.T) {
	steps := trancheFiveMigrations()
	idx := -1
	for i, step := range steps {
		if step[0] == "ddlWorkloadPseudonymKey" {
			idx = i
		}
	}
	if idx < 1 || steps[idx-1][0] != "ddlPlanApprovalModes" {
		t.Fatalf("T5 steps = %v, want ddlPlanApprovalModes then ddlWorkloadPseudonymKey", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.workload_pseudonym_key", "CHECK (id = 1)",
		"CHECK (sealed <> '')"} {
		if !strings.Contains(ddlWorkloadPseudonymKey, want) {
			t.Errorf("the step lacks %q", want)
		}
	}
	for _, stmt := range classifyStep(ddlWorkloadPseudonymKey) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

// Integration: two bootstraps leave one table that holds at most one row
// (id = 1) and refuses an empty value.
func TestWorkloadPseudonymKeyMigration_BootstrapTwiceAndConstraints(t *testing.T) {
	pool := freshSchemaPool(t, "pseudonymkey")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var cols string
	if err := pool.QueryRow(ctx, `SELECT string_agg(attname || ':' ||
		format_type(atttypid, atttypmod) || ':' || attnotnull, ',' ORDER BY attnum)
		FROM pg_attribute WHERE attrelid = 'sage.workload_pseudonym_key'::regclass
		AND attnum > 0 AND NOT attisdropped`).Scan(&cols); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	want := "id:integer:true,sealed:text:true,created_at:timestamp with time zone:true"
	if cols != want {
		t.Fatalf("columns = %s, want %s", cols, want)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.workload_pseudonym_key (id, sealed)
		VALUES (1, 'x')`); err != nil {
		t.Fatalf("a valid row was refused: %v", err)
	}
	for name, sql := range map[string]string{
		"a second row": `INSERT INTO sage.workload_pseudonym_key (id, sealed) VALUES (2, 'y')`,
		"empty sealed": `UPDATE sage.workload_pseudonym_key SET sealed = ''`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
