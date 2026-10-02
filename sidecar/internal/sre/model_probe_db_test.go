package sre

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The model's next probe (AI-SRE-SPEC §6/§11): only when the graph is
// inconclusive, one catalog probe with typed args, run within the probe
// ceiling, followed by a fresh deterministic diagnosis and a final
// review of it.

// retryLockRunner times out on the first lock graph probe and sees the
// idle-in-transaction chain on the next one.
func retryLockRunner() *scriptedRunner {
	chain := idleChainRunner().scripts[probes.LockGraph][0]
	return newScriptedRunner().script(probes.LockGraph, lockGraphTimeout(), chain)
}

const probeWhy = "re-reading the lock graph tells an idle holder from an active one"

// probeFirstReview ranks the open hypotheses, asks for the lock graph
// again and narrates the failed probe.
func probeFirstReview(t *testing.T) fakeReply {
	return toolReply(func(body string) string {
		return wireReview{Ranking: unknownRanking(),
			NextProbe: nextProbe("lock_graph", nil, probeWhy),
			Claims: []wireClaim{{Text: "The lock graph probe timed out.",
				EvidenceIDs: []string{aliasOf(t, body, probes.LockGraph, "error")}}}}.json()
	})
}

func stepNexts(t *testing.T, st *PostgresStore, inv Investigation) []string {
	t.Helper()
	var out []string
	for _, p := range payloads(t, st, inv, EventStep) {
		if key, _ := p["key"].(string); strings.HasPrefix(key, "model-") {
			out = append(out, p["next"].(string))
		}
	}
	return out
}

func TestModelProbe_InconclusiveGraphConcludesAfterTheProbe(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, probeFirstReview(t), toolReply(validIdleReview(t)))
	runner := retryLockRunner()
	c, _ := modelCoordinator(t, ctx, st, runner, m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-probe"))
	assertIdleRoot(t, st, inv)
	if inv.ProbeCount != 5 || runner.total() != 5 || inv.ModelTurns != 2 || m.calls() != 2 {
		t.Fatalf("probes=%d run=%d turns=%d calls=%d", inv.ProbeCount, runner.total(),
			inv.ModelTurns, m.calls())
	}
	p := inv.Summary.ModelProbe
	if p == nil || p.Label != ModelProbeLabel || p.ProbeID != "lock_graph" ||
		p.Rationale != probeWhy || p.EvidenceID != lockEvidenceID(t, st, inv, 2) {
		t.Fatalf("model probe = %+v", p)
	}
	if r := inv.Summary.ModelRanking; r == nil ||
		strings.Join(r.Nodes, ",") != "idle_in_tx_holder,ddl_lock_queue" {
		t.Fatalf("final ranking = %+v, want the review of the final diagnosis", r)
	}
	if got := strings.Join(stepNexts(t, st, inv), ","); got !=
		"needs_evidence,collecting,evaluating" {
		t.Fatalf("model probe transitions = %s", got)
	}
	if !strings.Contains(m.body(t, 1), "[lock_graph ok]") ||
		strings.Contains(m.body(t, 1), "window_seconds") {
		t.Fatalf("the final review must see the new evidence and offer no probe: %s",
			m.body(t, 1))
	}
	reviewed := payloads(t, st, inv, EventModelReviewed)
	if len(reviewed) != 1 || reviewed[0]["next_probe"] != "lock_graph" {
		t.Fatalf("model_reviewed = %v", reviewed)
	}
}

// When the final review fails (no repair turn is left), the first review
// is verified against the final diagnosis: its ranking is now stale and
// is dropped, its still-grounded claim is kept.
func TestModelProbe_FailedFinalReviewKeepsVerifiedFirstReview(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, probeFirstReview(t), contentReply(fixed(`{"ranking": [`)))
	c, _ := modelCoordinator(t, ctx, st, retryLockRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-probe-final"))
	assertIdleRoot(t, st, inv)
	if inv.Summary.ModelRanking != nil || inv.Summary.Narrative == nil ||
		len(inv.Summary.Narrative.Claims) != 1 || inv.Summary.ModelProbe == nil {
		t.Fatalf("summary = %+v", inv.Summary)
	}
	reasons := map[string]bool{}
	for _, p := range payloads(t, st, inv, EventModelRejected) {
		reasons[p["reason"].(string)] = true
	}
	if !reasons[RejectMalformed] || !reasons[RejectVerifier] || len(reasons) != 2 {
		t.Fatalf("rejections = %v, want the final review and the stale ranking", reasons)
	}
}

// §11: the model's probe counts against the 12-probe ceiling; at the cap
// it is not run, and the first review stands.
func TestModelProbe_ProbeCeilingHonored(t *testing.T) {
	limits := budgetLimits()
	limits.MaxProbes = 4
	st, _, ctx := liveStore(t, limits)
	m := newFakeModel(t, probeFirstReview(t), toolReply(validIdleReview(t)))
	runner := retryLockRunner()
	c, _ := modelCoordinator(t, ctx, st, runner, m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-probe-cap"))
	if inv.State != StateInconclusive || inv.ProbeCount != 4 || runner.total() != 4 {
		t.Fatalf("state=%s probes=%d run=%d", inv.State, inv.ProbeCount, runner.total())
	}
	if inv.Summary.ModelProbe != nil || inv.Summary.ModelRanking == nil ||
		inv.Summary.ModelRanking.Nodes[0] != "idle_in_tx_holder" || m.calls() != 1 {
		t.Fatalf("summary = %+v calls=%d", inv.Summary, m.calls())
	}
	rej := payloads(t, st, inv, EventModelRejected)
	if len(rej) != 1 || rej[0]["reason"] != RejectProbeBudget {
		t.Fatalf("model_rejected = %v", rej)
	}
}

// A resumed investigation whose turns a crashed worker already used does
// not call the provider again; it concludes deterministically.
func TestModelProbe_ResumedRunWithTurnsUsedFallsBack(t *testing.T) {
	limits := budgetLimits()
	limits.LeaseTTL = time.Second
	st, _, ctx := liveStore(t, limits)
	m := newFakeModel(t, toolReply(validIdleReview(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv, _, err := c.Start(ctx, lockTrigger("m3-resume"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	dead, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, key := range []string{"crashed-1", "crashed-2"} {
		if _, err := st.ReserveModel(ctx, dead, tokens(1000, 500, key)); err != nil {
			t.Fatalf("reserve %s: %v", key, err)
		}
	}
	time.Sleep(1200 * time.Millisecond) // the dead worker's lease expires
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("investigate: %v", err)
	}
	got, _ := st.Get(ctx, inv.Scope, inv.ID)
	assertIdleRoot(t, st, got)
	assertDeterministicOnly(t, got)
	rej := payloads(t, st, got, EventModelRejected)
	if m.calls() != 0 || got.ModelTurns != 2 || len(rej) != 1 ||
		rej[0]["reason"] != RejectBudget {
		t.Fatalf("calls=%d turns=%d rejected=%v", m.calls(), got.ModelTurns, rej)
	}
}
