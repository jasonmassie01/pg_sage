package schema

import (
	"context"
	"strings"
	"testing"
)

// cli-02b on a real server: bootstrap twice is clean; one change has at
// most one approval PR row; the state check refuses an unknown state.
// No concurrent-writer test: Bootstrap holds its advisory lock.
func TestApprovalPRsMigration_BootstrapTwiceOneRowPerChange(t *testing.T) {
	pool := freshSchemaPool(t, "approvalprs")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	insert := `INSERT INTO sage.approval_prs (database, queue_id, repo, path, card_hash,
		expires_at, state) VALUES ($1, $2, 'o/cfg', 'approvals/x.yaml', 'h',
		now() + interval '1 day', $3)`
	if _, err := pool.Exec(ctx, insert, "orders", 7, "opening"); err != nil {
		t.Fatalf("first row refused: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "billing", 7, "opening"); err != nil {
		t.Fatalf("same queue id on another database refused: %v", err)
	}
	_, err := pool.Exec(ctx, insert, "orders", 7, "opening")
	if err == nil || !strings.Contains(err.Error(), "approval_prs_one_per_change") {
		t.Fatalf("second row for one change accepted or wrong error: %v", err)
	}
	_, err = pool.Exec(ctx, insert, "orders", 8, "merged")
	if err == nil || !strings.Contains(err.Error(), "approval_prs_state") {
		t.Fatalf("unknown state accepted or wrong error: %v", err)
	}
	var state, detail string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT state, detail, attempts FROM sage.approval_prs
		WHERE database = 'orders' AND queue_id = 7`).Scan(&state, &detail,
		&attempts); err != nil || state != "opening" || detail != "" || attempts != 0 {
		t.Fatalf("row = %q %q %d (%v), want opening, empty detail, 0 attempts", state,
			detail, attempts, err)
	}
}
