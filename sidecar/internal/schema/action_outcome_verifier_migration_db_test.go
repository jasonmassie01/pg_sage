package schema

import (
	"context"
	"strings"
	"testing"
)

// lab-01c on a real server: bootstrap twice is clean, and
// sage.action_outcome gains verifier (v1 by default, v1 or v2 only),
// other_verdict and other_evidence. No concurrent-writer test: Bootstrap
// holds its advisory lock.
func TestActionOutcomeVerifierMigration_BootstrapTwiceAndConstraints(t *testing.T) {
	pool := freshSchemaPool(t, "outcomeverifier")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome) VALUES ('create_index', 'CREATE INDEX x ON t (a)', 'monitoring')
		RETURNING id`).Scan(&actionID); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_outcome (action_log_id,
		action_class, predicted, prediction_method) VALUES ($1, 'index_create', '{}',
		'none')`, actionID); err != nil {
		t.Fatalf("insert outcome: %v", err)
	}
	var verifier string
	var other *string
	if err := pool.QueryRow(ctx, `SELECT verifier, other_verdict FROM sage.action_outcome
		WHERE action_log_id = $1`, actionID).Scan(&verifier, &other); err != nil {
		t.Fatalf("read the new columns: %v", err)
	}
	if verifier != "v1" || other != nil {
		t.Fatalf("defaults: verifier %q other %v, want v1 and NULL", verifier, other)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_outcome SET verifier = 'v2',
		other_verdict = 'improved', other_evidence = '{"v2": {"method": "did"}}'
		WHERE action_log_id = $1`, actionID); err != nil {
		t.Fatalf("a v2 row was refused: %v", err)
	}
	_, err := pool.Exec(ctx, `UPDATE sage.action_outcome SET verifier = 'v3'
		WHERE action_log_id = $1`, actionID)
	if err == nil || !strings.Contains(err.Error(), "check") {
		t.Fatalf("verifier v3 must hit the check constraint, got %v", err)
	}
}
