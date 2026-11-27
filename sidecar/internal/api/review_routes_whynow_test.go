package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/review/rubric"
	"github.com/pg-sage/sidecar/internal/review/trigger"
)

// review-04a: the grade route's why_now is the triggers the latest run
// consumed, with their details; a plain grade has none.

type whyNowBody struct {
	Latest *rubric.Run       `json:"latest"`
	WhyNow []trigger.Trigger `json:"why_now"`
}

// seedTriggeredReview records a finished review run that consumed a
// pre_upgrade trigger and coalesced a large_migration one.
func seedTriggeredReview(t *testing.T, ctx context.Context, rs *rubric.Store,
	ts *trigger.Store) (int64, []int64) {
	t.Helper()
	run, err := rs.Start(ctx, rubric.RunSpec{Database: "orders", Kind: rubric.KindReview,
		Triggers: []string{"large_migration", "pre_upgrade"}, RubricVersion: rubric.Version})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	var ids []int64
	for i, tr := range []trigger.Trigger{
		{Kind: trigger.KindPreUpgrade, Key: "major:18", Detail: map[string]any{
			"target_major": 18}},
		{Kind: trigger.KindLargeMigration, Key: "migration:x", Detail: map[string]any{
			"large_table_bytes": 1024}},
	} {
		tr.Database, tr.FiredAt = "orders", at.Add(time.Duration(i)*time.Minute)
		id, ok, err := ts.Fire(ctx, tr)
		if err != nil || !ok {
			t.Fatalf("fire %s: %v %v", tr.Kind, ok, err)
		}
		ids = append(ids, id)
	}
	if _, err := ts.Claim(ctx, "orders", ids, "test", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := ts.Attach(ctx, ids, run, "test"); err != nil {
		t.Fatal(err)
	}
	g := rubric.Compute(rubric.V1(), nil)
	if err := rs.Finish(ctx, run, rubric.RunDone, &g, ""); err != nil {
		t.Fatal(err)
	}
	if err := ts.Consume(ctx, ids, run); err != nil {
		t.Fatal(err)
	}
	return run, ids
}

func TestReviewGradeRouteDB_WhyNowListsTheLatestRunsTriggers(t *testing.T) {
	pool := auditPool(t, "review_why_now")
	ctx := context.Background()
	h := reviewMux(t, pool)
	code, body := drillGet(t, h, "/api/v1/databases/orders/review/grade")
	var empty whyNowBody
	if code != http.StatusOK || json.Unmarshal(body, &empty) != nil || empty.WhyNow == nil ||
		len(empty.WhyNow) != 0 {
		t.Fatalf("no run yet = %d %s, want why_now []", code, body)
	}
	rs := rubric.NewStore(pool)
	seedGrade(t, rs, nil)
	code, body = drillGet(t, h, "/api/v1/databases/orders/review/grade")
	var grade whyNowBody
	if code != http.StatusOK || json.Unmarshal(body, &grade) != nil || grade.Latest == nil ||
		grade.WhyNow == nil || len(grade.WhyNow) != 0 {
		t.Fatalf("grade = %d %s, want a grade with why_now []", code, body)
	}
	run, ids := seedTriggeredReview(t, ctx, rs, trigger.NewStore(pool))
	code, body = drillGet(t, h, "/api/v1/databases/orders/review/grade")
	var got whyNowBody
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Latest == nil ||
		got.Latest.ID != run || len(got.WhyNow) != 2 {
		t.Fatalf("review = %d %s, want run %d with two triggers", code, body, run)
	}
	first, second := got.WhyNow[0], got.WhyNow[1]
	if first.ID != ids[0] || first.Kind != trigger.KindPreUpgrade ||
		first.State != trigger.StateConsumed || first.Detail["target_major"] != float64(18) ||
		second.ID != ids[1] || second.Kind != trigger.KindLargeMigration ||
		second.State != trigger.StateCoalesced || second.CoalescedInto != ids[0] ||
		second.RunID != run {
		t.Fatalf("why_now = %+v, want pre_upgrade consumed then large_migration coalesced",
			got.WhyNow)
	}
}
