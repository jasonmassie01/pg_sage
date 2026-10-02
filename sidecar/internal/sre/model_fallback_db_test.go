package sre

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Validation + one repair + deterministic fallback (AI-SRE-SPEC §11,
// CHECK-11/12): every way a reply can be wrong ends, after one failed
// repair, in the deterministic conclusion and one model_rejected event
// naming the reason. The model never fails or blocks an investigation.

// unknownLockRunner scripts a lock investigation whose lock graph probe
// always times out, so the graph stays inconclusive.
func unknownLockRunner() *scriptedRunner {
	return newScriptedRunner().script(probes.LockGraph, lockGraphTimeout())
}

type fallbackCase struct {
	reply      fakeReply
	reason     string
	runner     func() *scriptedRunner
	conclusive bool
}

func fallbackCases(t *testing.T) map[string]fallbackCase {
	review := func(w wireReview) fakeReply { return toolReply(fixed(w.json())) }
	probeReview := func(probe map[string]any) fakeReply {
		return review(wireReview{Ranking: unknownRanking(), NextProbe: probe})
	}
	why := "tells an idle holder from an active one"
	return map[string]fallbackCase{
		"malformed JSON": {contentReply(fixed(`{"ranking": [`)), RejectMalformed,
			idleChainRunner, true},
		"empty response": {contentReply(fixed("")), RejectEmpty, idleChainRunner, true},
		"unknown node": {review(wireReview{Ranking: []string{"cosmic_rays"}}),
			RejectUnknownNode, idleChainRunner, true},
		"ungrounded claim": {toolReply(func(body string) string {
			return wireReview{Ranking: idleRanking(), Claims: []wireClaim{{Text: "pid " +
				"4242 blocks 7 sessions.", EvidenceIDs: []string{aliasOf(t, body,
				probes.LockGraph, "ok")}}}}.json()
		}), RejectClaims, idleChainRunner, true},
		"uncited claim": {review(wireReview{Ranking: idleRanking(), Claims: []wireClaim{{
			Text: "The holder is idle."}}}), RejectClaims, idleChainRunner, true},
		"oversized output": {contentReply(fixed(`{"ranking":[],"claims":[],"x":"` +
			strings.Repeat("a", maxModelOutputBytes) + `"}`)), RejectOversized,
			idleChainRunner, true},
		"probe on a conclusive graph": {review(wireReview{Ranking: idleRanking(),
			NextProbe: nextProbe("lock_graph", nil, why)}), RejectProbeNotAllowed,
			idleChainRunner, true},
		"undeclared tool": {callTool("run_sql", fixed(`{"sql":"DROP TABLE orders"}`)),
			RejectMalformed, idleChainRunner, true},
		"unknown probe": {probeReview(nextProbe("drop_table", nil, why)),
			RejectUnknownProbe, unknownLockRunner, false},
		"bad probe args": {probeReview(nextProbe("lock_graph",
			map[string]any{"pid": 7}, why)), RejectProbeArgs, unknownLockRunner, false},
	}
}

func TestModelTurn_RepairFailsFallsBackDeterministically(t *testing.T) {
	for name, tc := range fallbackCases(t) {
		t.Run(name, func(t *testing.T) {
			st, _, ctx := liveStore(t, budgetLimits())
			m := newFakeModel(t, tc.reply, tc.reply)
			runner := tc.runner()
			c, _ := modelCoordinator(t, ctx, st, runner, m.client())
			inv := startAndRun(t, ctx, c, lockTrigger("m3-fb-"+name))
			if tc.conclusive {
				assertIdleRoot(t, st, inv)
			} else if inv.State != StateInconclusive {
				t.Fatalf("state = %s, want inconclusive", inv.State)
			}
			assertDeterministicOnly(t, inv)
			if m.calls() != 2 || inv.ModelTurns != 2 || runner.total() != 4 ||
				inv.ProbeCount != 4 || inv.Summary.ModelProbe != nil {
				t.Fatalf("calls=%d turns=%d probes run=%d stored=%d, want 2 turns and "+
					"only the 4 plan probes", m.calls(), inv.ModelTurns, runner.total(),
					inv.ProbeCount)
			}
			rej := payloads(t, st, inv, EventModelRejected)
			if len(rej) != 1 || rej[0]["reason"] != tc.reason {
				t.Fatalf("model_rejected = %v, want reason %s", rej, tc.reason)
			}
			if len(payloads(t, st, inv, EventModelReviewed)) != 0 {
				t.Fatal("a rejected review was recorded as accepted")
			}
		})
	}
}

// Without a daily allocation the provider is never called and no turn is
// charged; the investigation is the deterministic one.
func TestModelTurn_NoAllocationNeverCallsTheProvider(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	m := newFakeModel(t, toolReply(validIdleReview(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-noalloc"))
	assertIdleRoot(t, st, inv)
	assertDeterministicOnly(t, inv)
	rej := payloads(t, st, inv, EventModelRejected)
	if m.calls() != 0 || inv.ModelTurns != 0 || len(rej) != 1 ||
		rej[0]["reason"] != RejectBudget {
		t.Fatalf("calls=%d turns=%d rejected=%v", m.calls(), inv.ModelTurns, rej)
	}
}

// A disabled LLM client (no provider configured, or turned off at run
// time) makes no call, records nothing per investigation, and is logged
// once however many investigations run.
func TestModelTurn_DisabledClientIsLoggedOnce(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, toolReply(validIdleReview(t)))
	c, logs := modelCoordinator(t, ctx, st, idleChainRunner(), llmClient(m.srv.URL, false))
	for _, subject := range []string{"m3-off-1", "m3-off-2"} {
		inv := startAndRun(t, ctx, c, lockTrigger(subject))
		assertIdleRoot(t, st, inv)
		assertDeterministicOnly(t, inv)
		for _, typ := range []string{EventModelRejected, EventModelReviewed} {
			if got := payloads(t, st, inv, typ); len(got) != 0 {
				t.Fatalf("%s events for a disabled model: %v", typ, got)
			}
		}
		if inv.ModelTurns != 0 {
			t.Fatalf("a disabled model consumed %d turns", inv.ModelTurns)
		}
	}
	if m.calls() != 0 || logs.count("model turn unavailable") != 1 {
		t.Fatalf("calls=%d notices=%d, want 0 calls and one notice", m.calls(),
			logs.count("model turn unavailable"))
	}
}

// §11 wall budget: with too little active time left the model is not
// called and the investigation concludes deterministically.
func TestModelTurn_NoTimeLeftSkipsTheModel(t *testing.T) {
	limits := budgetLimits()
	limits.MaxActive, limits.LeaseTTL = 4*time.Second, 4*time.Second
	st, _, ctx := liveStore(t, limits)
	m := newFakeModel(t, toolReply(validIdleReview(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-notime"))
	assertIdleRoot(t, st, inv)
	rej := payloads(t, st, inv, EventModelRejected)
	if m.calls() != 0 || inv.ModelTurns != 0 || len(rej) != 1 ||
		rej[0]["reason"] != RejectNoTime {
		t.Fatalf("calls=%d turns=%d rejected=%v", m.calls(), inv.ModelTurns, rej)
	}
}

// Two workers racing for one investigation: one claims it and runs the
// model turn once; the durable turn count is the one that worker used.
func TestModelTurn_ConcurrentWorkersReviewOnce(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, toolReply(validIdleReview(t)), toolReply(validIdleReview(t)))
	a, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	b, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	b.cfg.RuntimeKey = a.cfg.RuntimeKey
	b.bound = false
	if _, err := b.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	inv, _, err := a.Start(ctx, lockTrigger("m3-race"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var wg sync.WaitGroup
	for _, c := range []*Coordinator{a, b} {
		wg.Add(1)
		go func(c *Coordinator) {
			defer wg.Done()
			if err := c.Investigate(ctx, inv.ID); err != nil {
				t.Errorf("investigate: %v", err)
			}
		}(c)
	}
	wg.Wait()
	got, _ := st.Get(ctx, inv.Scope, inv.ID)
	assertIdleRoot(t, st, got)
	if m.calls() != 1 || got.ModelTurns != 1 ||
		len(payloads(t, st, got, EventModelReviewed)) != 1 {
		t.Fatalf("calls=%d turns=%d, want exactly one review", m.calls(), got.ModelTurns)
	}
}
