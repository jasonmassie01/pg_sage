package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/recommendation"
)

// neutralSettledRecommendation applies fx's recommendation and settles its
// action the way the executor settles a neutral verdict: outcome success
// with a completed unverifiable verification. It returns the action id.
func neutralSettledRecommendation(t *testing.T, fx recAPIFixture) int64 {
	t.Helper()
	ctx := t.Context()
	recs := recommendation.NewStore(fx.pool)
	approved, err := recs.Approve(ctx, fx.rec.ID, fx.rec.ContentHash, "user:2")
	if err != nil {
		t.Fatal(err)
	}
	c, err := recs.Claim(ctx, recommendation.ClaimRequest{ID: approved.ID,
		Revision: approved.Revision, Lease: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var decisionID, actionID int64
	if err := fx.pool.QueryRow(ctx, `INSERT INTO sage.decision (feature, intent,
		verdict, risk_tier, reason, evidence_id) VALUES ('index', 'create_index',
		'execute', 'safe', 't', md5(random()::text)) RETURNING id`).
		Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	if err := fx.pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type,
		finding_id, sql_executed, outcome, decision_id) VALUES ('create_index', $1,
		$2, 'monitoring', $3) RETURNING id`, fx.find, fx.p.ForwardSQL, decisionID).
		Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	if err := recs.RecordApplied(ctx, c, actionID); err != nil {
		t.Fatal(err)
	}
	if err := recs.StartVerifying(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(ctx, `WITH v AS (INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline, minimum_samples,
		 next_evaluation_at, hard_deadline_at, verdict, reason, completed_at)
		VALUES ($2, $1, '{"kind":"executor_postcheck"}', '{}', 1, now(), now(),
		        'unverifiable', 'neutral', now()) RETURNING id)
		UPDATE sage.action_log SET outcome='success', rollback_reason='neutral',
		measured_at=now(), verification_id=(SELECT id FROM v) WHERE id=$1`,
		actionID, decisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := recs.ReconcileVerifying(ctx, fx.dbName); err != nil {
		t.Fatal(err)
	}
	return actionID
}

// CHECK-27 (cases-03): after one neutral action the recommendation API and
// the Cases timeline both say inconclusive; neither says verified.
func TestNeutralActionIsInconclusiveOnRecommendationsAndCases(t *testing.T) {
	fx := newRecAPIFixture(t)
	actionID := neutralSettledRecommendation(t, fx)

	w := detailRequest(fx, strconv.FormatInt(fx.rec.ID, 10))
	if w.Code != http.StatusOK {
		t.Fatalf("detail status %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Recommendation recommendation.Recommendation `json:"recommendation"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Recommendation.State != recommendation.StateInconclusive ||
		body.Recommendation.Verdict != "unverifiable" {
		t.Fatalf("recommendation API: state=%s verdict=%q, want inconclusive/unverifiable",
			body.Recommendation.State, body.Recommendation.Verdict)
	}

	logs, err := queryActionLogsByFindingIDs(t.Context(), fx.pool, []int{fx.find})
	if err != nil {
		t.Fatalf("case action logs: %v", err)
	}
	if len(logs[fx.find]) != 1 {
		t.Fatalf("case action logs for finding %d = %d rows, want 1",
			fx.find, len(logs[fx.find]))
	}
	action := caseActionFromActionLog(logs[fx.find][0])
	if action.ID != "log:"+strconv.FormatInt(actionID, 10) ||
		action.VerificationStatus != "inconclusive" {
		t.Fatalf("cases timeline action %s: verification=%q, want log:%d inconclusive",
			action.ID, action.VerificationStatus, actionID)
	}
}
