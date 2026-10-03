package executor

import (
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

// Dogfood lifeos (stale approval): an optimizer CREATE INDEX was queued for
// approval because its HypoPG what-if was unverified. HypoPG re-verified
// it hours later, but the executor kept gating on the recommendation
// revision's original (unverified) evidence and the pending approval stayed
// parked until it expired. When the reason for approval disappears, the
// next cycle must supersede the queued proposal and run the change once
// through the normal autonomous path.

// 1. Re-verified: superseded on the next cycle, executed exactly once.
func TestStaleApprovalSupersededWhenWhatIfVerified(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	if got := fx.decisionVerdicts(t)["queue_approval/approval_required"]; got != 1 {
		t.Fatalf("decisions = %v, want one queue_approval/approval_required row",
			fx.decisionVerdicts(t))
	}
	fx.markVerified(t)

	fx.exec.RunCycle(fx.ctx, false)

	after := fx.onlyQueueRow(t)
	if after.ID != q.ID || after.Status != "superseded" ||
		!strings.Contains(after.Reason, "approval no longer required: what-if verified") {
		t.Fatalf("queue row = %+v, want %d superseded because what-if verified", after, q.ID)
	}
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 || !fx.indexExists(t, fx.index()) {
		t.Fatalf("actions=%d index=%v, want one execution that built the index",
			n, fx.indexExists(t, fx.index()))
	}
	if got := fx.decisionVerdicts(t)["execute/authorized"]; got == 0 {
		t.Fatalf("decisions = %v, want an execute verdict", fx.decisionVerdicts(t))
	}
	head, err := fx.recs.Get(fx.ctx, fx.rec.ID)
	if err != nil || head.State == recommendation.StateProposed || head.ActionLogID == nil {
		t.Fatalf("recommendation = %+v (%v), want claimed and linked to its action", head, err)
	}
	fx.exec.RunCycle(fx.ctx, false)
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 || len(fx.queueRows(t)) != 1 {
		t.Fatalf("after another cycle: actions=%d queue=%+v, want 1 action, 1 row",
			n, fx.queueRows(t))
	}
}

// 2. Still unverified: the proposal stays pending and nothing executes.
func TestStaleApprovalStillUnverifiedStaysPending(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	still := unverifiedDetail()
	still["what_if_reason"] = "HypoPG measured no improvement above the floor"
	fx.setFinding(t, fx.f.RecommendedSQL, still)
	for range 2 {
		fx.exec.RunCycle(fx.ctx, false)
	}
	if after := fx.onlyQueueRow(t); after != q {
		t.Fatalf("queue row = %+v, want unchanged %+v", after, q)
	}
	if fx.actions(t, fx.f.RecommendedSQL) != 0 || fx.indexExists(t, fx.index()) {
		t.Fatal("an unverified index ran without approval")
	}
	if got := fx.decisionVerdicts(t); got["execute/authorized"] != 0 {
		t.Fatalf("decisions = %v, want no execute verdict", got)
	}
}

// A finding row without any verdict (written before verdicts existed)
// fails closed even when the revision is unverified too.
func TestStaleApprovalLegacyFindingWithoutVerdictStaysPending(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	fx.setFinding(t, fx.f.RecommendedSQL, map[string]any{"queryids": []int64{7}})
	fx.exec.RunCycle(fx.ctx, false)
	if after := fx.onlyQueueRow(t); after != q || fx.actions(t, fx.f.RecommendedSQL) != 0 {
		t.Fatalf("queue row = %+v actions=%d, want unchanged and none", after,
			fx.actions(t, fx.f.RecommendedSQL))
	}
}

// 3a. An operator rejection is never overridden by autonomy: the change
// keeps needing approval, also after the re-queue cooldown.
func TestStaleApprovalRejectedStaysRejected(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	if err := fx.queue.Reject(fx.ctx, q.ID, 1, "not on this table"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	fx.markVerified(t)
	fx.exec.RunCycle(fx.ctx, false)
	rejected := fx.onlyQueueRow(t)
	if rejected.Status != "rejected" || rejected.Reason != "not on this table" {
		t.Fatalf("queue row = %+v, want the rejection unchanged", rejected)
	}
	if fx.actions(t, fx.f.RecommendedSQL) != 0 || fx.indexExists(t, fx.index()) {
		t.Fatal("a rejected change ran autonomously")
	}
	// Past the re-queue cooldown the normal rule proposes it again for an
	// operator; it still never runs unattended.
	fx.setQueue(t, q.ID, "decided_at = now() - interval '2 hours'")
	fx.exec.RunCycle(fx.ctx, false)
	rows := fx.queueRows(t)
	if len(rows) != 2 || rows[0].Status != "rejected" || rows[1].Status != "pending" {
		t.Fatalf("queue rows = %+v, want the rejection kept and a new pending proposal", rows)
	}
	if fx.actions(t, fx.f.RecommendedSQL) != 0 {
		t.Fatal("a rejected change ran autonomously after the cooldown")
	}
}

// 3b. An approved proposal belongs to the operator path: autonomy neither
// supersedes it nor runs the change beside it.
func TestStaleApprovalApprovedIsLeftToTheOperatorPath(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	fx.setQueue(t, q.ID, "status = 'approved', decided_at = now()")
	fx.markVerified(t)
	fx.exec.RunCycle(fx.ctx, false)
	if after := fx.onlyQueueRow(t); after.Status != "approved" || after.Reason != "" {
		t.Fatalf("queue row = %+v, want approved and untouched", after)
	}
	if fx.actions(t, fx.f.RecommendedSQL) != 0 || fx.indexExists(t, fx.index()) {
		t.Fatal("autonomy ran a change an operator approval owns")
	}
}

// 3c. An expired proposal stays expired; the authorized change runs once.
func TestStaleApprovalExpiredIsUnchangedAndChangeRuns(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	fx.setQueue(t, q.ID, "status = 'expired', expires_at = now() - interval '1 second', "+
		"reason = 'action proposal expired'")
	fx.markVerified(t)
	fx.exec.RunCycle(fx.ctx, false)
	after := fx.onlyQueueRow(t)
	if after.Status != "expired" || after.Reason != "action proposal expired" {
		t.Fatalf("queue row = %+v, want the expiry unchanged", after)
	}
	if fx.actions(t, fx.f.RecommendedSQL) != 1 || !fx.indexExists(t, fx.index()) {
		t.Fatalf("actions=%d, want the authorized change run once",
			fx.actions(t, fx.f.RecommendedSQL))
	}
}

// 4a. Revised content: the queued proposal of the old revision is
// superseded by the revision, only the new SQL ever runs.
func TestStaleApprovalRevisedContentRunsOnlyNewSQL(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	newIndex := fx.index() + "_v2"
	revised := fx.indexFinding(newIndex, verifiedDetail())
	fx.setFinding(t, revised.RecommendedSQL, verifiedDetail())
	res, err := fx.recs.Propose(fx.ctx, analyzer.RecommendationProposal(fx.database, revised))
	if err != nil || res.Outcome != recommendation.OutcomeRevised {
		t.Fatalf("revise: %+v, %v", res, err)
	}
	fx.exec.RunCycle(fx.ctx, false)
	old := fx.queueRows(t)[0]
	if old.ID != q.ID || old.Status != "superseded" ||
		!strings.Contains(old.Reason, "revised to revision 2") {
		t.Fatalf("old proposal = %+v, want superseded by the revision", old)
	}
	if fx.actions(t, fx.f.RecommendedSQL) != 0 || fx.indexExists(t, fx.index()) {
		t.Fatal("the stale revision's SQL ran")
	}
	if fx.actions(t, revised.RecommendedSQL) != 1 || !fx.indexExists(t, newIndex) {
		t.Fatalf("new revision actions=%d, want 1", fx.actions(t, revised.RecommendedSQL))
	}
}

// 4b. The finding moved on to other SQL before the recommendation was
// revised: its verdict is about that SQL and must not authorize the old.
func TestStaleApprovalVerdictForOtherSQLDoesNotAuthorize(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	other := fx.indexFinding(fx.index()+"_other", verifiedDetail())
	fx.setFinding(t, other.RecommendedSQL, verifiedDetail())
	fx.exec.RunCycle(fx.ctx, false)
	if after := fx.onlyQueueRow(t); after != q {
		t.Fatalf("queue row = %+v, want unchanged %+v", after, q)
	}
	if fx.actions(t, fx.f.RecommendedSQL) != 0 || fx.actions(t, other.RecommendedSQL) != 0 {
		t.Fatal("a verdict about other SQL authorized an execution")
	}
}

// 4c. A pending proposal for different content (a legacy row) is not this
// change's approval: it is left alone and nothing runs beside it.
func TestStaleApprovalPendingForOtherContentBlocks(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	otherSQL := "CREATE INDEX CONCURRENTLY " + fx.index() + "_legacy ON public." +
		fx.table + " USING btree (status, id);"
	fx.setQueue(t, q.ID, "proposed_sql = $2, content_hash = NULL, "+
		"recommendation_id = NULL, recommendation_revision = NULL", otherSQL)
	fx.markVerified(t)
	fx.exec.RunCycle(fx.ctx, false)
	after := fx.onlyQueueRow(t)
	if after.Status != "pending" || after.Reason != "" || after.ProposedSQL != otherSQL {
		t.Fatalf("queue row = %+v, want the other proposal untouched", after)
	}
	if fx.actions(t, fx.f.RecommendedSQL) != 0 || fx.actions(t, otherSQL) != 0 {
		t.Fatal("a change ran beside a pending proposal for other content")
	}
}

// 5. Concurrent cycles: superseded once, executed once.
func TestStaleApprovalConcurrentCyclesExecuteOnce(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	q := fx.queuePending(t)
	fx.markVerified(t)
	executors := []*Executor{fx.exec, fx.newExecutor(t), fx.newExecutor(t)}
	var start, done sync.WaitGroup
	start.Add(1)
	for _, e := range executors {
		done.Add(1)
		go func(e *Executor) {
			defer done.Done()
			start.Wait()
			e.RunCycle(fx.ctx, false)
		}(e)
	}
	start.Done()
	done.Wait()
	after := fx.onlyQueueRow(t)
	if after.ID != q.ID || after.Status != "superseded" ||
		!strings.Contains(after.Reason, "approval no longer required") {
		t.Fatalf("queue row = %+v, want superseded once", after)
	}
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 || fx.applyingTransitions(t) != 1 {
		t.Fatalf("actions=%d applying=%d, want exactly one execution",
			n, fx.applyingTransitions(t))
	}
}

// 6. Generic: approval needed only because trust was advisory; once the
// operator raises trust, the queued proposal is superseded and runs once.
func TestStaleApprovalTrustRaisedSupersedes(t *testing.T) {
	fx := newStaleFixtureWith(t, "advisory", verifiedDetail())
	q := fx.queuePending(t)
	if err := fx.exec.SetTrustLevel("autonomous"); err != nil {
		t.Fatalf("raise trust: %v", err)
	}
	fx.exec.RunCycle(fx.ctx, false)
	after := fx.onlyQueueRow(t)
	if after.ID != q.ID || after.Status != "superseded" ||
		!strings.Contains(after.Reason, "approval no longer required") ||
		strings.Contains(after.Reason, "what-if") {
		t.Fatalf("queue row = %+v, want superseded by the standing policy", after)
	}
	if fx.actions(t, fx.f.RecommendedSQL) != 1 || !fx.indexExists(t, fx.index()) {
		t.Fatal("the authorized change did not run exactly once")
	}
}

// A worker that read the candidate before another worker ran it, and got
// past the lease after that run, must not run it again (nor record a
// failed attempt because the index now exists).
func TestStaleApprovalLateWorkerDoesNotRunAgain(t *testing.T) {
	fx := newStaleFixture(t, "autonomous")
	fx.queuePending(t)
	fx.markVerified(t)
	cands := fx.exec.actionableRecommendations(fx.ctx)
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	stale := cands[0]
	fx.exec.RunCycle(fx.ctx, false)
	if fx.actions(t, fx.f.RecommendedSQL) != 1 {
		t.Fatal("the first worker did not run the change")
	}
	late := fx.newExecutor(t)
	f := late.currentGateEvidence(fx.ctx, findingFromCandidate(fx.database, stale),
		fx.findingID, &stale)
	decision := ActionPolicyDecision{Decision: PolicyDecisionExecute,
		DecisionID: recordCustodianDecision(t, fx.ctx, fx.pool, "index", fx.table)}
	if id := late.runAuthorizedFinding(fx.ctx, f, fx.findingID, decision, &stale); id != 0 {
		t.Fatalf("late worker recorded action %d, want none", id)
	}
	if n := fx.actions(t, fx.f.RecommendedSQL); n != 1 || fx.applyingTransitions(t) != 1 {
		t.Fatalf("actions=%d applying=%d, want the single first run", n,
			fx.applyingTransitions(t))
	}
}
