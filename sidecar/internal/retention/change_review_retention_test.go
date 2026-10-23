package retention

import (
	"context"
	"testing"
)

// CHECK-FND-09: complete change reviews older than retention.actions_days
// are purged; rows of the same age that still hold a pending credit or
// wait to land survive, as does a review still running; budget day rows
// older than the window are purged.

func TestPurgeRulesAgeOutChangeReviews(t *testing.T) {
	found := map[string]purgeRule{}
	for _, rule := range purgeRules(allDays(45)) {
		if rule.table == "change_review" || rule.table == "change_review_budget_day" {
			found[rule.table] = rule
		}
	}
	cr, ok := found["change_review"]
	if !ok || cr.timeCol != "updated_at" || cr.days != 45 || cr.extra != keepChangeReview {
		t.Fatalf("change_review rule = %+v", cr)
	}
	bd, ok := found["change_review_budget_day"]
	if !ok || bd.timeCol != "day" || bd.days != 45 {
		t.Fatalf("change_review_budget_day rule = %+v", bd)
	}
}

func TestRun_ChangeReviewsAgeOutUnlessStillOwed(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("cr_ret")
	insert := func(state, credit, landing, age string) string {
		var id string
		err := testPool.QueryRow(ctx, `INSERT INTO sage.change_review (database_name, source,
			actor, input_hash, state, verdict, deterministic_verdict, parse_mode,
			model_status, landing_state, receipt_verdict, credit_state, credit_final_at,
			completed_at, created_at, updated_at)
			VALUES ('db', 'cli', $1, repeat('a', 64), $2,
			CASE WHEN $2 = 'complete' THEN 'safe' END,
			CASE WHEN $2 = 'complete' THEN 'safe' END,
			CASE WHEN $2 = 'complete' THEN 'ast' END,
			CASE WHEN $2 = 'complete' THEN 'ok' END,
			NULLIF($4, ''), CASE WHEN $2 = 'complete' THEN 'pending' END, $3,
			CASE WHEN $3 = 'final' THEN now() - $5::interval END,
			CASE WHEN $2 = 'complete' THEN now() - $5::interval END,
			now() - $5::interval, now() - $5::interval) RETURNING id::text`,
			tag, state, credit, landing, age).Scan(&id)
		if err != nil {
			t.Fatalf("seed review: %v", err)
		}
		return id
	}
	finalOld := insert("complete", "final", "not_applicable", "40 days")
	pendingOld := insert("complete", "pending", "not_applicable", "40 days")
	awaitingOld := insert("complete", "none", "awaiting", "40 days")
	runningOld := insert("running", "none", "", "40 days")
	failedOld := insert("failed", "none", "", "40 days")
	finalFresh := insert("complete", "final", "not_applicable", "2 days")
	if _, err := testPool.Exec(ctx, `INSERT INTO sage.change_review_budget_day
		(day, actor, tokens) VALUES (current_date - 40, $1, 10), (current_date, $1, 20)`,
		tag); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM sage.change_review WHERE actor = $1`, tag)
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM sage.change_review_budget_day WHERE actor = $1`, tag)
	})
	New(testPool, allDays(30), noopLog).Run(ctx)
	for id, kept := range map[string]bool{finalOld: false, failedOld: false,
		pendingOld: true, awaitingOld: true, runningOld: true, finalFresh: true} {
		n := countWhere(t, ctx, `SELECT count(*) FROM sage.change_review WHERE id = $1::uuid`,
			id)
		if (n == 1) != kept {
			t.Errorf("review %s kept=%d, want %v", id, n, kept)
		}
	}
	if got := countWhere(t, ctx, `SELECT count(*) FROM sage.change_review_budget_day
		WHERE actor = $1`, tag); got != 1 {
		t.Fatalf("budget rows left = %d, want today's only", got)
	}
}
