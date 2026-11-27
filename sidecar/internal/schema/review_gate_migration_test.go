package schema

import (
	"context"
	"strings"
	"testing"
)

// review-05a: sage.review_gate records every G-M1 and G-M2 evaluation, the
// approval of a G-M1 grant and its revocation. No concurrent-writer test
// here: Bootstrap holds its advisory lock; two evaluations at once are
// tested in internal/review/modelgate.

// review-01b's ddlReviewViews was merged after this step, so the pin is
// "ddlReviewPlans then ddlReviewGate" rather than "ddlReviewGate last".
func TestReviewGateStep_IsAT6StepIdempotentAndBounded(t *testing.T) {
	steps := trancheSixMigrations()
	at := stepIndex(steps, "ddlReviewGate")
	if at < 1 || steps[at][1] != ddlReviewGate || steps[at-1][0] != "ddlReviewPlans" {
		t.Fatalf("T6 steps %v, want ddlReviewPlans then ddlReviewGate", steps)
	}
	for _, want := range []string{"SET LOCAL lock_timeout = '5s'",
		"CREATE TABLE IF NOT EXISTS sage.review_gate (",
		"rule IN ('G-M1', 'G-M2')", "UNIQUE (model, rule, tick)",
		"verdict <> 'granted' OR approved_at IS NOT NULL",
		"CREATE INDEX IF NOT EXISTS review_gate_latest"} {
		if !strings.Contains(ddlReviewGate, want) {
			t.Errorf("ddlReviewGate lacks %q", want)
		}
	}
	for _, bad := range []string{"UPDATE sage.", "DELETE FROM", "DROP TABLE"} {
		if strings.Contains(ddlReviewGate, bad) {
			t.Errorf("ddlReviewGate runs %q", bad)
		}
	}
	for _, stmt := range classifyStep(ddlReviewGate) {
		if stmt.shape == shapeUnknown {
			t.Errorf("statement %q is not classified", stmt.sql)
		}
	}
}

// Integration: two bootstraps are clean, the ledger holds the step once,
// and the table refuses a grant without an approval (CHECK-39), a second
// row for one model, rule and tick, and unknown words.
func TestReviewGateMigration_BootstrapTwiceAndChecked(t *testing.T) {
	pool := freshSchemaPool(t, "reviewgate")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlReviewGate'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlReviewGate: %d (%v), want 1", ledger, err)
	}
	insert := `INSERT INTO sage.review_gate (model, rule, tick, verdict, reason,
		approved_by, approved_at) VALUES ($1, $2, $3::timestamptz, $4, 'r', $5,
		CASE WHEN $5::text IS NULL THEN NULL ELSE now() END)`
	tick := "2026-11-17T00:00:00Z"
	if _, err := pool.Exec(ctx, insert, "m", "G-M1", tick, "pending_approval",
		nil); err != nil {
		t.Fatalf("a pending row was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "m", "G-M2", tick, "model_on", nil); err != nil {
		t.Fatalf("a G-M2 row of the same tick was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "m", "G-M1", "2026-11-18T00:00:00Z", "granted",
		"admin@example.com"); err != nil {
		t.Fatalf("an approved grant was refused: %v", err)
	}
	for name, args := range map[string][]any{
		"granted without approval": {"m", "G-M1", "2026-11-19T00:00:00Z", "granted", nil},
		"second row of one tick":   {"m", "G-M1", tick, "not_eligible", nil},
		"unknown rule":             {"m", "G-M3", tick, "not_eligible", nil},
		"unknown verdict":          {"m", "G-M1", "2026-11-20T00:00:00Z", "maybe", nil},
		"empty model":              {"", "G-M1", "2026-11-21T00:00:00Z", "eligible", nil},
		"empty approver":           {"m", "G-M1", "2026-11-22T00:00:00Z", "granted", ""},
	} {
		if _, err := pool.Exec(ctx, insert, args...); err == nil {
			t.Errorf("%s: the gate row was accepted", name)
		}
	}
}
