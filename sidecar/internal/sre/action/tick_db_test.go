package action

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The action service's background tick against real PostgreSQL: queue
// decisions flow back to proposals, recovery is verified from fresh
// samples, abandoned runs are never retried, automatic proposals honour
// configuration, and outcomes are readable for earned autonomy.

func TestActionTickSyncsDenialAndExpiry(t *testing.T) {
	h := newActionHarness(t, nil)
	p := h.requested(t)
	if err := h.as.Reject(h.ctx, p.QueueID, 5, "not now"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := h.actions.Tick(h.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := h.proposal(t, p.ID)
	if got.State != ProposalDenied || got.DecidedBy != 5 || got.Detail != "not now" {
		t.Fatalf("denied proposal = %+v", got)
	}
	// The approval anchor (a critical finding) does not linger open.
	var findingStatus string
	_ = h.pool.QueryRow(h.ctx, `SELECT status FROM sage.findings WHERE id = $1`,
		got.FindingID).Scan(&findingStatus)
	if findingStatus != "resolved" {
		t.Fatalf("denied proposal's finding is %q, want resolved", findingStatus)
	}

	h2 := newActionHarness(t, nil)
	p2 := h2.requested(t)
	if _, err := h2.pool.Exec(h2.ctx, `UPDATE sage.action_queue
		SET expires_at = now() - interval '1 second' WHERE id = $1`, p2.QueueID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if err := h2.actions.Tick(h2.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := h2.proposal(t, p2.ID); got.State != ProposalExpired {
		t.Fatalf("expired item left the proposal %s", got.State)
	}
	if _, err := h2.as.Approve(h2.ctx, p2.QueueID, 3); err == nil {
		t.Fatal("an expired approval item was approved")
	}
}

func executeAndObserve(t *testing.T, h *actionHarness, recovery probes.Result) Proposal {
	t.Helper()
	p := h.requested(t)
	if _, err := h.actions.RunApproved(h.ctx, h.approved(t, p, 3), 3); err != nil {
		t.Fatalf("RunApproved: %v", err)
	}
	h.targets.script(probes.RecoverySample, recovery)
	for i := 0; i < 6; i++ {
		h.clock.advance(h.cfg.RecoveryInterval)
		if err := h.actions.Tick(h.ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	return h.proposal(t, p.ID)
}

func TestActionTickVerifiesRecovery(t *testing.T) {
	h := newActionHarness(t, nil)
	got := executeAndObserve(t, h, recoveryRows(false))
	if got.Recovery.State != RecoveryRecovered || len(got.Recovery.Samples) != 3 {
		t.Fatalf("recovery = %+v, want recovered after 3 samples", got.Recovery)
	}
	types := h.eventTypes(t)
	if countOf(types, "recovery_sample") != 3 || countOf(types, "recovery_verdict") != 1 {
		t.Fatalf("events = %v", types)
	}
	status, err := h.queue.Status(h.ctx, got.QueueID)
	if err != nil || status.VerificationStatus != "verified" {
		t.Fatalf("queue verification = %+v, %v", status, err)
	}
}

func TestActionTickReportsNotRecovered(t *testing.T) {
	h := newActionHarness(t, nil)
	got := executeAndObserve(t, h, recoveryRows(true))
	if got.Recovery.State != RecoveryNotRecovered ||
		!strings.Contains(got.Recovery.Verdict, "still blocks") {
		t.Fatalf("recovery = %+v, want not_recovered", got.Recovery)
	}
	if status, _ := h.queue.Status(h.ctx, got.QueueID); status.VerificationStatus != "failed" {
		t.Fatalf("queue verification = %+v, want failed", status)
	}
}

// A crash between claiming and recording leaves "executing": the service
// never retries a possibly completed signal; it marks the outcome
// uncertain and verifies recovery instead.
func TestActionTickMarksAbandonedExecutionUncertain(t *testing.T) {
	h := newActionHarness(t, nil)
	p := h.requested(t)
	h.approved(t, p, 3)
	if _, err := h.pool.Exec(h.ctx, `UPDATE sage.sre_action_proposals
		SET state = 'executing', updated_at = clock_timestamp() - interval '10 minutes'
		WHERE id = $1`, string(p.ID)); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}
	if err := h.actions.Tick(h.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := h.proposal(t, p.ID)
	if got.State != ProposalUncertain || got.Recovery.State != RecoveryObserving ||
		h.cancel.callCount() != 0 {
		t.Fatalf("abandoned execution = %s / %s, cancels %d", got.State,
			got.Recovery.State, h.cancel.callCount())
	}
}

func TestActionTickAutoProposesAndRequestsOnce(t *testing.T) {
	h := newActionHarness(t, nil, func(c *ActionConfig) { c.RequestApproval = true })
	for i := 0; i < 3; i++ {
		if err := h.actions.Tick(h.ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	ps, err := h.actions.ForInvestigation(h.ctx, h.inv.ID)
	if err != nil || len(ps) != 1 || ps[0].State != ProposalRequested {
		t.Fatalf("auto proposals = %+v, %v", ps, err)
	}
	if h.queueRows(t, ps[0].ID) != 1 || h.notes.count() != 1 || h.cancel.callCount() != 0 {
		t.Fatalf("queue %d notes %d cancels %d", h.queueRows(t, ps[0].ID), h.notes.count(),
			h.cancel.callCount())
	}
}

func TestActionTickHonoursDisabledProposals(t *testing.T) {
	h := newActionHarness(t, nil, func(c *ActionConfig) { c.Proposals = false })
	if err := h.actions.Tick(h.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if ps, _ := h.actions.ForInvestigation(h.ctx, h.inv.ID); len(ps) != 0 {
		t.Fatalf("disabled automatic proposals created %+v", ps)
	}
	if p, err := h.actions.Propose(h.ctx, h.inv.ID, "user:1"); err != nil ||
		p.State != ProposalProposed {
		t.Fatalf("an operator's explicit proposal = %+v, %v", p, err)
	}
}

// Outcomes feed M7's per-family autonomy ledger.
func TestActionOutcomesReportVerifiedResults(t *testing.T) {
	h := newActionHarness(t, nil)
	got := executeAndObserve(t, h, recoveryRows(false))
	outs, err := h.actions.Outcomes(h.ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(outs) != 1 {
		t.Fatalf("outcomes = %+v, %v", outs, err)
	}
	o := outs[0]
	if o.ProposalID != got.ID || o.Family != "lock_blocking" || o.Class != ActionCancelBackend ||
		o.State != ProposalExecuted || o.Recovery != RecoveryRecovered ||
		o.Reversibility != executor.ReversibilityMitigationOnly {
		t.Fatalf("outcome = %+v", o)
	}
	classes := ActionClasses()
	if len(classes) != 1 || classes[0].Class != ActionCancelBackend ||
		classes[0].MaxLevel != "L2" {
		t.Fatalf("action classes = %+v", classes)
	}
}
