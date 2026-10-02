package sre

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The model turn inside a real investigation (Sage SRE M3): real store,
// scripted probes, a fake OpenAI-compatible model. The deterministic
// diagnosis is always persisted; the model's ranking and narrative are
// stored beside it only when they survive validation and the verifier.

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logFn(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+msg)
}

func (l *logLines) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

// modelCoordinator is testCoordinator with a model client and a private
// once-log for model notices.
func modelCoordinator(t *testing.T, ctx context.Context, st *PostgresStore,
	runner ProbeRunner, model *llm.Client) (*Coordinator, *logLines) {
	t.Helper()
	logs := &logLines{}
	cfg := DefaultCoordinatorConfig("test:" + string(NewUUID()))
	c, err := NewCoordinator(CoordinatorDeps{Store: st, Runner: runner, Config: cfg,
		Model: model, Notices: &OnceLog{}, LogFn: logs.logFn})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if _, err := c.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return c, logs
}

// payloads returns the decoded payloads of an investigation's events of
// one type, in order.
func payloads(t *testing.T, st *PostgresStore, inv Investigation, typ string) []map[string]any {
	t.Helper()
	events, err := st.Events(t.Context(), inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var out []map[string]any
	for _, e := range events {
		if e.Type != typ {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("event payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func lockEvidenceID(t *testing.T, st *PostgresStore, inv Investigation, n int) UUID {
	t.Helper()
	ev, err := st.Evidence(t.Context(), inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	seen := 0
	for _, e := range ev {
		if e.ProbeID == string(probes.LockGraph) {
			if seen++; seen == n {
				return e.ID
			}
		}
	}
	t.Fatalf("no lock_graph evidence #%d in %d rows", n, len(ev))
	return ""
}

func assertDeterministicOnly(t *testing.T, inv Investigation) {
	t.Helper()
	if inv.Summary.ModelRanking != nil || inv.Summary.Narrative != nil {
		t.Fatalf("model output stored: ranking=%+v narrative=%+v",
			inv.Summary.ModelRanking, inv.Summary.Narrative)
	}
}

func assertIdleRoot(t *testing.T, st *PostgresStore, inv Investigation) {
	t.Helper()
	if inv.State != StateConcluded || inv.Summary.Root != "idle_in_tx_holder" {
		t.Fatalf("investigation = %s root %q, want concluded idle_in_tx_holder",
			inv.State, inv.Summary.Root)
	}
	hs, _ := st.Hypotheses(t.Context(), inv.Scope, inv.ID)
	statuses := map[string]HypothesisStatus{}
	for _, h := range hs {
		statuses[h.Node] = h.Status
	}
	if statuses["idle_in_tx_holder"] != HypothesisRoot ||
		statuses["ddl_lock_queue"] != HypothesisContributing {
		t.Fatalf("deterministic statuses changed: %v", statuses)
	}
}

func TestModelTurn_AgreeingReviewIsStoredLabeled(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, toolReply(validIdleReview(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-agree"))
	assertIdleRoot(t, st, inv)
	r := inv.Summary.ModelRanking
	if r == nil || r.Label != ModelRankingLabel || r.Basis != ModelRankingBasis ||
		strings.Join(r.Nodes, ",") != "idle_in_tx_holder,ddl_lock_queue" {
		t.Fatalf("model ranking = %+v", r)
	}
	n := inv.Summary.Narrative
	if n == nil || n.Label != NarrativeLabel || len(n.Claims) != 1 ||
		n.Claims[0].EvidenceIDs[0] != lockEvidenceID(t, st, inv, 1) {
		t.Fatalf("narrative = %+v", n)
	}
	if inv.ModelTurns != 1 || m.calls() != 1 || inv.Summary.ModelProbe != nil {
		t.Fatalf("turns=%d calls=%d probe=%+v", inv.ModelTurns, m.calls(),
			inv.Summary.ModelProbe)
	}
	reviewed := payloads(t, st, inv, EventModelReviewed)
	if len(reviewed) != 1 || reviewed[0]["claims"] != float64(1) ||
		len(payloads(t, st, inv, EventModelRejected)) != 0 {
		t.Fatalf("model events: reviewed=%v", reviewed)
	}
	body := m.body(t, 0)
	if !strings.Contains(body, `"tools"`) || strings.Count(body, `"name":"`) != 1 ||
		!strings.Contains(body, reviewToolName) {
		t.Fatalf("request must offer exactly the review tool: %s", body)
	}
	if err := st.VerifyEvents(ctx, inv.Scope, inv.ID); err != nil {
		t.Fatalf("event chain: %v", err)
	}
}

// The model's ranking never changes the deterministic confidence.
func TestModelTurn_ConfidenceMatchesTheDeterministicRun(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	plain, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	base := startAndRun(t, ctx, plain, lockTrigger("m3-base"))
	m := newFakeModel(t, toolReply(validIdleReview(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-conf"))
	want, _ := st.Hypotheses(ctx, base.Scope, base.ID)
	got, _ := st.Hypotheses(ctx, inv.Scope, inv.ID)
	if len(got) != len(want) || len(got) == 0 {
		t.Fatalf("hypotheses %d vs %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Node != want[i].Node || got[i].Status != want[i].Status ||
			got[i].Confidence != want[i].Confidence || got[i].Ordinal != want[i].Ordinal {
			t.Fatalf("hypothesis %d = %+v, deterministic %+v", i, got[i], want[i])
		}
	}
}

func TestModelTurn_MarkdownWrappedJSONAccepted(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	review := validIdleReview(t)
	m := newFakeModel(t, contentReply(func(body string) string {
		return "```json\n" + review(body) + "\n```"
	}))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-fenced"))
	if inv.Summary.ModelRanking == nil || inv.Summary.Narrative == nil || m.calls() != 1 {
		t.Fatalf("fenced JSON not accepted: %+v (calls %d)", inv.Summary, m.calls())
	}
}

func TestModelTurn_RepairSucceeds(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	bad := wireReview{Ranking: []string{"idle_in_tx_holder", "cosmic_rays"}}.json()
	m := newFakeModel(t, toolReply(fixed(bad)), toolReply(validIdleReview(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-repair"))
	assertIdleRoot(t, st, inv)
	if inv.Summary.ModelRanking == nil || inv.ModelTurns != 2 || m.calls() != 2 {
		t.Fatalf("repaired review not stored: %+v turns=%d calls=%d",
			inv.Summary.ModelRanking, inv.ModelTurns, m.calls())
	}
	if !strings.Contains(m.body(t, 1), RejectUnknownNode) ||
		!strings.Contains(m.body(t, 1), "cosmic_rays") {
		t.Fatalf("repair request does not say what was wrong: %s", m.body(t, 1))
	}
	reviewed := payloads(t, st, inv, EventModelReviewed)
	if len(reviewed) != 1 || reviewed[0]["repaired"] != RejectUnknownNode ||
		len(payloads(t, st, inv, EventModelRejected)) != 0 {
		t.Fatalf("events: reviewed=%v", reviewed)
	}
}

// CHECK-11: rate limits and timeouts get no repair attempt; the
// investigation concludes deterministically at once.
func TestModelTurn_TimeoutAndRateLimitFallBackWithoutRepair(t *testing.T) {
	cases := map[string]struct {
		reply  fakeReply
		reason string
	}{
		"timeout":      {slowReply(2*time.Second, toolReply(fixed(minimalReview))), RejectTimeout},
		"rate limited": {statusReply(http.StatusTooManyRequests), RejectRateLimited},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st, _, ctx := liveStore(t, budgetLimits())
			m := newFakeModel(t, tc.reply, toolReply(fixed(minimalReview)))
			c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
			c.cfg.ModelTimeout = 300 * time.Millisecond
			started := time.Now()
			inv := startAndRun(t, ctx, c, lockTrigger("m3-"+name))
			assertIdleRoot(t, st, inv)
			assertDeterministicOnly(t, inv)
			if m.calls() != 1 || inv.ModelTurns != 1 {
				t.Fatalf("calls=%d turns=%d, want one attempt and no repair", m.calls(),
					inv.ModelTurns)
			}
			rej := payloads(t, st, inv, EventModelRejected)
			if len(rej) != 1 || rej[0]["reason"] != tc.reason {
				t.Fatalf("model_rejected = %v, want reason %s", rej, tc.reason)
			}
			if time.Since(started) > 10*time.Second {
				t.Fatalf("fallback took %s", time.Since(started))
			}
		})
	}
}

// §11: a provider that rejects tool calling gets the repair turn as
// JSON-schema prompting, without tools.
func TestModelTurn_ProviderWithoutToolsGetsJSONPrompt(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, statusReply(http.StatusBadRequest),
		contentReply(validIdleReview(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-notools"))
	if inv.Summary.ModelRanking == nil || inv.Summary.Narrative == nil ||
		inv.ModelTurns != 2 {
		t.Fatalf("JSON-prompted review not stored: %+v turns=%d", inv.Summary,
			inv.ModelTurns)
	}
	if !strings.Contains(m.body(t, 0), `"tools"`) {
		t.Fatalf("first request had no tools: %s", m.body(t, 0))
	}
	second := m.body(t, 1)
	if strings.Contains(second, `"tools"`) || !strings.Contains(second, `next_probe`) {
		t.Fatalf("fallback request must carry the schema and no tools: %s", second)
	}
}

// The model may not change a conclusive root cause (graph wins).
func TestModelTurn_ConclusiveRootCannotChange(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, toolReply(func(body string) string {
		return wireReview{Ranking: []string{"ddl_lock_queue", "idle_in_tx_holder"},
			Claims: []wireClaim{idleClaim(aliasOf(t, body, probes.LockGraph, "ok"))}}.json()
	}))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-override"))
	assertIdleRoot(t, st, inv)
	assertDeterministicOnly(t, inv)
	got := payloads(t, st, inv, EventModelDisagreed)
	if len(got) != 1 || got[0]["graph_root"] != "idle_in_tx_holder" ||
		got[0]["model_root"] != "ddl_lock_queue" || m.calls() != 1 {
		t.Fatalf("model_disagreed = %v (calls %d)", got, m.calls())
	}
	if len(payloads(t, st, inv, EventModelReviewed)) != 0 {
		t.Fatal("a disagreeing review was recorded as accepted")
	}
}
