package schema

import (
	"context"
	"strings"
	"testing"
)

// lab-03b on a real server: bootstrap twice is clean, and
// sage.rollout_instance gains effect_pct, control_pct and effect_method
// (one of the three effect methods, or NULL for a row from before). No
// concurrent-writer test: Bootstrap holds its advisory lock.
func TestRolloutInstanceEffectMigration_BootstrapTwiceAndChecked(t *testing.T) {
	pool := freshSchemaPool(t, "rollouteffect")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.rollout_run (evidence_id,
		source_instance, prior_evidence_id, policy, state)
		VALUES ('canary-lab03b', 'src', 'action_log:src:1', '{}', 'canary')`); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.rollout_instance (run_evidence_id,
		instance_id, ordinal, status, effect_pct, control_pct, effect_method)
		VALUES ('canary-lab03b', 'a', 1, 'applied', -30.5, 0.25, 'v2_fleet_control'),
		       ('canary-lab03b', 'b', 2, 'applied', 4, NULL, 'v2'),
		       ('canary-lab03b', 'c', 3, 'applied', NULL, NULL, 'flag_only'),
		       ('canary-lab03b', 'd', 4, 'applied', NULL, NULL, NULL)`); err != nil {
		t.Fatalf("insert the three methods and a pre-migration row: %v", err)
	}
	var effect, control float64
	var method string
	if err := pool.QueryRow(ctx, `SELECT effect_pct, control_pct, effect_method
		FROM sage.rollout_instance WHERE instance_id = 'a'`).Scan(&effect, &control,
		&method); err != nil || effect != -30.5 || control != 0.25 ||
		method != "v2_fleet_control" {
		t.Fatalf("stored effect = %v %v %q (%v)", effect, control, method, err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.rollout_instance (run_evidence_id,
		instance_id, ordinal, status, effect_method)
		VALUES ('canary-lab03b', 'e', 5, 'applied', 'guess')`)
	if err == nil || !strings.Contains(err.Error(), "rollout_instance_effect_method") {
		t.Fatalf("unknown effect method accepted or wrong error: %v", err)
	}
}
