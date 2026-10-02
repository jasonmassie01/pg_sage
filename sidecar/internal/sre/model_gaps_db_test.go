package sre

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Gaps found by mutating the model turn after the first test pass: each
// test here failed against one mutant that the earlier tests let live.

// A claim grounded only in facts the final diagnosis no longer binds to
// its evidence (the lock graph was re-read and superseded) is dropped.
func TestVerifyReview_ClaimsGroundedInSupersededFactsDropped(t *testing.T) {
	inv, before, d0 := idleChainFixture(t)
	other := idleChainRunner().Run(t.Context(), probes.LockGraph, probes.Args{})
	for i := range other.Rows {
		other.Rows[i]["blocker_pid"] = int64(5151)
	}
	after := append(append([]Evidence(nil), before...), fixtureEvidence(t, other)...)
	final := diagnoseEvidence(t, inv, after)
	if final.Root == nil || final.Subject != "pid 5151" {
		t.Fatalf("final diagnosis = %+v", final)
	}
	r := groundedReview(t, d0, before, idleRanking()...)
	v := verifyReview(r, final, after)
	if len(v.review.Claims) != 0 {
		t.Fatalf("a claim about pid 4242 survived its superseded evidence: %+v", v.review)
	}
}

// An investigation resumed after it already moved to evaluating once
// and then needed more evidence (here under the v1.7 "evaluate" step
// key) reaches evaluating again and concludes.
func TestCollect_ResumeAfterNeedsEvidenceConcludes(t *testing.T) {
	limits := budgetLimits()
	limits.LeaseTTL = time.Second
	st, _, ctx := liveStore(t, limits)
	runner := idleChainRunner()
	c, _ := modelCoordinator(t, ctx, st, runner, nil)
	inv, _, err := c.Start(ctx, lockTrigger("m3-reeval"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	dead, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	plan, _ := planFor(TriggerLock, time.Hour)
	steps := []StepResult{{IdempotencyKey: "step-1", Results: c.runStep(ctx, plan[0]),
		NextState: StateCollecting}, {IdempotencyKey: "evaluate",
		NextState: StateEvaluating}, {IdempotencyKey: "model-probe-old",
		Results:   []probes.Result{runner.Run(ctx, probes.LockGraph, probes.Args{})},
		NextState: StateNeedsEvidence}}
	for _, step := range steps {
		if _, err := st.CommitStep(ctx, dead, step); err != nil {
			t.Fatalf("commit %s: %v", step.IdempotencyKey, err)
		}
	}
	time.Sleep(1200 * time.Millisecond) // the dead worker's lease expires
	if err := c.Investigate(ctx, inv.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := st.Get(ctx, inv.Scope, inv.ID)
	assertIdleRoot(t, st, got)
}

// After a repaired first review that asked for a probe, no turn is left:
// the probe runs, no final review is attempted (no budget rejection), and
// the verifier drops the now-stale ranking.
func TestModelProbe_RepairedReviewLeavesNoFinalTurn(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	bad := wireReview{Ranking: []string{"cosmic_rays"}}.json()
	m := newFakeModel(t, toolReply(fixed(bad)), probeFirstReview(t))
	c, _ := modelCoordinator(t, ctx, st, retryLockRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-repair-probe"))
	assertIdleRoot(t, st, inv)
	if m.calls() != 2 || inv.ModelTurns != 2 || inv.ProbeCount != 5 ||
		inv.Summary.ModelProbe == nil {
		t.Fatalf("calls=%d turns=%d probes=%d probe=%+v", m.calls(), inv.ModelTurns,
			inv.ProbeCount, inv.Summary.ModelProbe)
	}
	rej := payloads(t, st, inv, EventModelRejected)
	if len(rej) != 1 || rej[0]["reason"] != RejectVerifier {
		t.Fatalf("model_rejected = %v, want only the stale ranking", rej)
	}
}

// The store refuses model output citing evidence of another
// investigation.
func TestConclude_RefusesModelEvidenceOutOfScope(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), nil)
	other := startAndRun(t, ctx, c, lockTrigger("m3-other"))
	foreign, _ := st.Evidence(ctx, other.Scope, other.ID)
	inv, _, err := c.Start(ctx, lockTrigger("m3-scope"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	lease, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	plan, _ := planFor(TriggerLock, time.Hour)
	if _, err := st.CommitStep(ctx, lease, StepResult{IdempotencyKey: "step-1",
		Results: c.runStep(ctx, plan[0]), NextState: StateEvaluating}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ev, _ := st.Evidence(ctx, inv.Scope, inv.ID)
	d := diagnoseEvidence(t, inv, ev)
	cases := map[string]func(*Conclusion){
		"narrative": func(cn *Conclusion) {
			cn.Summary.Narrative = &Narrative{Label: NarrativeLabel, Claims: []NarrativeClaim{
				{Text: "The holder is idle.", EvidenceIDs: []UUID{foreign[0].ID}}}}
		},
		"model probe": func(cn *Conclusion) {
			cn.Summary.ModelProbe = &ModelProbe{Label: ModelProbeLabel,
				ProbeID: "lock_graph", Rationale: "x", EvidenceID: foreign[0].ID}
		},
	}
	for name, mutate := range cases {
		cn := conclusionOf(d)
		mutate(&cn)
		if _, err := st.Conclude(ctx, lease, cn); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s citing another investigation = %v, want ErrInvalidRequest",
				name, err)
		}
	}
}

// The client turned off during the call (hot reload): no event, one
// notice, the deterministic conclusion.
func TestModelTurn_DisabledDuringTheCall(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	var c *Coordinator
	m := newFakeModel(t, func(w http.ResponseWriter, body string) {
		c.model.Reconfigure(&config.LLMConfig{Enabled: false})
		toolReply(validIdleReview(t))(w, body)
	})
	c, logs := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-reload"))
	assertIdleRoot(t, st, inv)
	assertDeterministicOnly(t, inv)
	if got := payloads(t, st, inv, EventModelRejected); len(got) != 0 {
		t.Fatalf("a disabled client was recorded per investigation: %v", got)
	}
	if logs.count("model turn unavailable") != 1 || m.calls() != 1 {
		t.Fatalf("notices=%d calls=%d", logs.count("model turn unavailable"), m.calls())
	}
}

// Model text is redacted before it is stored, not only when it is read.
func TestModelSurface_StoredNarrativeIsRedacted(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, secretClaimReview(t))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-stored"))
	n := inv.Summary.Narrative
	if n == nil || len(n.Claims) != 1 || strings.Contains(n.Claims[0].Text, "hunter2") ||
		!strings.Contains(n.Claims[0].Text, "pid 4242 blocks 2") {
		t.Fatalf("stored narrative = %+v", n)
	}
}

// Every evidence line sits inside the evidence data block.
func TestReviewMessages_EvidenceInsideItsFence(t *testing.T) {
	inv, ev, d := idleChainFixture(t)
	user := reviewMessages(inv, newReviewScope(d, ev, false), "", true)[1].Content
	open := strings.Index(user, `<data label="evidence">`)
	if open < 0 {
		t.Fatalf("no evidence data block:\n%s", user)
	}
	end := open + strings.Index(user[open:], "</data>")
	for _, alias := range []string{"E1 [", "E2 [", "E3 ["} {
		at := strings.Index(user, alias)
		if at < open || at > end {
			t.Fatalf("%s is outside the evidence block:\n%s", alias, user)
		}
	}
}

// Pins a limitation: a reasoning model gets 16384 extra output tokens
// from the LLM client, more than the 4k per-investigation ceiling, so its
// turn is refused before any request is sent.
func TestModelTurn_ReasoningModelRefusedBeforeDispatch(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, toolReply(validIdleReview(t)))
	client := llm.New(&config.LLMConfig{Enabled: true, Endpoint: m.srv.URL, APIKey: "k",
		Model: "gemini-2.5-flash", TimeoutSeconds: 5, TokenBudgetDaily: 1_000_000},
		func(string, string, ...any) {})
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), client)
	inv := startAndRun(t, ctx, c, lockTrigger("m3-reasoning"))
	assertIdleRoot(t, st, inv)
	assertDeterministicOnly(t, inv)
	rej := payloads(t, st, inv, EventModelRejected)
	if m.calls() != 0 || len(rej) != 1 || rej[0]["reason"] != RejectBudget {
		t.Fatalf("calls=%d rejected=%v", m.calls(), rej)
	}
}
