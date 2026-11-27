package scorecard

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

// Any rollback of a step is a revert, whatever the executor got to: rolled
// back, still rolling back, or a rollback that failed. A step with a live
// step approval is accepted before it starts.
func TestStoreDB_EveryRollbackIsARevertAndAStepApprovalAccepts(t *testing.T) {
	pool, ctx := livePool(t)
	run := seed(t, ctx, pool, reviewtestReview(5))
	outcomes := []string{"rolled_back", "rolling_back", "rollback_failed", "success"}
	for i, outcome := range outcomes {
		if _, err := pool.Exec(ctx, `WITH a AS (INSERT INTO sage.action_log (action_type,
			sql_executed, outcome) VALUES ('create_index', 'CREATE INDEX x ON t (a)', $1)
			RETURNING id)
			UPDATE sage.plan_step s SET action_log_id = (SELECT id FROM a)
			FROM sage.review_plan rp WHERE rp.run_id = $2 AND s.plan_id = rp.plan_id
			AND s.ordinal = $3`, outcome, run, i+1); err != nil {
			t.Fatalf("link a %s action to step %d: %v", outcome, i+1, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.plan_step_approval (plan_id, step_id,
		plan_hash, approver_user_id, via) SELECT s.plan_id, s.id, repeat('a', 64), 7, 'cli'
		FROM sage.plan_step s JOIN sage.review_plan rp ON rp.plan_id = s.plan_id
		WHERE rp.run_id = $1 AND s.ordinal = 5`, run); err != nil {
		t.Fatalf("approve step 5: %v", err)
	}
	sc, err := NewStore(pool).Read(ctx, "shop", time.Time{}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	m := sc.ByOrigin[OriginModel]
	if m.Proposed != 5 || m.Reverted != 3 || m.Decided != 3 || m.Accepted != 1 {
		t.Fatalf("model counts = %+v, want 5 proposed, 3 reverted (every rollback), "+
			"3 decided and 1 accepted (the step approval)", m)
	}
}

func reviewtestReview(steps int) reviewtest.Review {
	r := reviewtest.Review{Database: "shop", Models: []string{"m1"}}
	for i := 0; i < steps; i++ {
		r.Steps = append(r.Steps, reviewtest.Outcome{Origin: "model", Ref: "m1"})
	}
	return r
}

// Only a done model run makes a review's model pass finished, and a step of
// a model plan with no origin row counts as the model's (fail closed).
func TestStoreDB_FailedModelRunsAndMissingOriginsFailClosed(t *testing.T) {
	pool, ctx := livePool(t)
	run := seed(t, ctx, pool, reviewtest.Review{Database: "shop", Steps: []reviewtest.Outcome{
		{Origin: "model", Ref: "m1", Verdict: "improved", Accepted: true}}})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.review_model_run (run_id, attempt, role,
		model, prompt_version, pack_hash, output_sha256, status) VALUES ($1, 1, 'rank', 'm1',
		'review-03a/1', $2, $2, 'failed')`, run, reviewtest.Hex64); err != nil {
		t.Fatalf("seed a failed model run: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.plan_step_origin WHERE run_id = $1`,
		run); err != nil {
		t.Fatalf("drop the step's origin: %v", err)
	}
	sc, err := NewStore(pool).Read(ctx, "shop", time.Time{}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if sc.Reviews != 1 || len(sc.PerReview[0].Run.Models) != 0 {
		t.Fatalf("review = %+v, want one review whose model pass did not finish", sc)
	}
	if m := sc.ByOrigin[OriginModel]; m.Proposed != 1 || m.Improved != 1 ||
		len(sc.ByOrigin) != 1 {
		t.Fatalf("by origin = %+v, want the origin-less step counted as the model's",
			sc.ByOrigin)
	}
}
