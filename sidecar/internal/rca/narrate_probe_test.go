package rca

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sage SRE M1: narration reads the probe catalog. The engine runs the
// incident family's catalog probes (never model-chosen SQL), the causal
// graph turns them into deterministic hypotheses, and both become
// evidence items (P#, H#) the model may read and must cite. The claim
// validator binds every number to the evidence its claim cites.

type fakeProbes struct {
	mu      sync.Mutex
	ran     []probes.ID
	results map[probes.ID]probes.Result
}

func (f *fakeProbes) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, id)
	if r, ok := f.results[id]; ok {
		return r
	}
	return probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
		ObservedAt: time.Now()}
}

func (f *fakeProbes) calls() []probes.ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]probes.ID(nil), f.ran...)
}

// idleGraph: pid 4242 idle in transaction for 75.5 s blocks an ALTER
// (pid 5001) that blocks a reader (pid 5002).
func idleGraph() *fakeProbes {
	start := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	edge := func(waiter, blocker int64, mode, state string, age float64,
		waiting bool) probes.Row {
		return probes.Row{"waiter_pid": waiter, "lock_type": "relation",
			"requested_mode": mode, "relation": "public.accounts",
			"blocker_pid": blocker, "blocker_kind": "backend",
			"blocker_state": state, "blocker_waiting": waiting,
			"blocker_xact_age_s": age, "blocker_backend_start": start}
	}
	cols := []string{"waiter_pid", "lock_type", "requested_mode", "relation",
		"blocker_pid", "blocker_kind", "blocker_state", "blocker_waiting",
		"blocker_xact_age_s", "blocker_backend_start"}
	return &fakeProbes{results: map[probes.ID]probes.Result{
		probes.LockGraph: {ProbeID: probes.LockGraph, Version: "v1",
			Status: probes.StatusOK, Columns: cols, ObservedAt: time.Now(),
			Rows: []probes.Row{
				edge(5001, 4242, "AccessExclusiveLock", "idle in transaction", 75.5, false),
				edge(5002, 5001, "AccessShareLock", "active", 1, true)}},
	}}
}

func probedEngine(url string, narrate bool, fp ProbeRunner) *Engine {
	eng := narrEngine(url, narrate)
	eng.WithProbes(fp)
	return eng
}

func TestNarrate_DeterministicCarriesCausalHypothesis(t *testing.T) {
	fp := idleGraph()
	eng := testEngine()
	eng.WithProbes(fp)
	n := eng.narrate(context.Background(), lockIncident(t))
	if n.Source != NarrationDeterministic {
		t.Fatalf("source = %s", n.Source)
	}
	for _, want := range []string{"Likely (H1, confidence 0.85): idle-in-transaction " +
		"holder, pid 4242", "contributing (H2): DDL queued behind a long transaction"} {
		if !strings.Contains(n.Text, want) {
			t.Fatalf("deterministic text lacks %q:\n%s", want, n.Text)
		}
	}
	ran := fp.calls()
	for _, want := range []probes.ID{probes.LockGraph, probes.LockChains,
		probes.LongTransactions, probes.PreparedXacts} {
		if !containsProbe(ran, want) {
			t.Fatalf("probes run = %v, want %s", ran, want)
		}
	}
	if len(ran) > probes.MaxProbesPerSignal {
		t.Fatalf("ran %d probes for one incident", len(ran))
	}
}

func containsProbe(ids []probes.ID, want probes.ID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestNarrate_UnavailableProbeIsNamedNotHidden(t *testing.T) {
	fp := &fakeProbes{results: map[probes.ID]probes.Result{
		probes.LockGraph: {ProbeID: probes.LockGraph, Version: "v1",
			Status: probes.StatusNoPrivilege, Reason: "insufficient_privilege"}}}
	eng := testEngine()
	eng.WithProbes(fp)
	n := eng.narrate(context.Background(), lockIncident(t))
	if !strings.Contains(n.Text, "Missing: lock_graph (no_privilege)") {
		t.Fatalf("deterministic text hides the denied probe:\n%s", n.Text)
	}
	// The incident's own blocker evidence (E2) still supports a hypothesis.
	if !strings.Contains(n.Text, "Likely (H1, confidence 0.7): idle-in-transaction") {
		t.Fatalf("fallback to incident evidence missing:\n%s", n.Text)
	}
}

func probeCall(id, probe string) []map[string]any {
	return []map[string]any{{"id": id, "type": "function", "function": map[string]any{
		"name": "get_probe_result", "arguments": `{"probe":"` + probe + `"}`}}}
}

const claimsFinal = `{"claims": [` +
	`{"text": "Session 4242 has been idle in transaction for 75.5 s.", ` +
	`"evidence_ids": ["P1"]}, ` +
	`{"text": "Likely cause: an idle-in-transaction holder (confidence 0.85).", ` +
	`"evidence_ids": ["H1"]}]}`

func TestNarrate_ModelReadsProbeResultsAndCitesThem(t *testing.T) {
	fp := idleGraph()
	s := newNarrServer(t, func(n int, w http.ResponseWriter) {
		if n == 1 {
			writeCompletion(w, "", probeCall("c1", "lock_graph"))
			return
		}
		writeCompletion(w, claimsFinal, nil)
	})
	n := probedEngine(s.srv.URL, true, fp).narrate(context.Background(), lockIncident(t))
	if n.Source != NarrationLLM || n.FallbackReason != "" {
		t.Fatalf("narration = %+v", n)
	}
	if strings.Join(n.Citations, ",") != "P1,H1" || !strings.Contains(n.Text, "75.5") {
		t.Fatalf("citations/text = %v %q", n.Citations, n.Text)
	}
	msgs := s.body(1)["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	content, _ := last["content"].(string)
	if last["role"] != "tool" || !strings.Contains(content, "lock_graph v1 ok") ||
		!strings.Contains(content, "<data") {
		t.Fatalf("probe result not returned as fenced evidence: %v", last)
	}
	user := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "P1: lock_graph") || !strings.Contains(user, "H1:") {
		t.Fatalf("prompt does not index probe and hypothesis evidence:\n%s", user)
	}
}

func TestNarrate_ClaimNumbersBindToTheirOwnCitations(t *testing.T) {
	cases := map[string]struct{ final, reason string }{
		// 75.5 is in P1, but this claim cites only E2.
		"number from uncited probe": {`{"claims": [{"text": "Idle for 75.5 s.", ` +
			`"evidence_ids": ["E2"]}]}`, "ungrounded number 75.5"},
		"unknown probe id": {`{"claims": [{"text": "Blocking.", ` +
			`"evidence_ids": ["P9"]}]}`, "unknown evidence"},
		"rounded number": {`{"claims": [{"text": "Idle for 75 s.", ` +
			`"evidence_ids": ["P1"]}]}`, "ungrounded number 75"},
		"no claims": {`{"claims": []}`, "no claims"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newNarrServer(t, func(_ int, w http.ResponseWriter) {
				writeCompletion(w, tc.final, nil)
			})
			n := probedEngine(s.srv.URL, true, idleGraph()).narrate(
				context.Background(), lockIncident(t))
			if n.Source != NarrationDeterministic ||
				!strings.Contains(n.FallbackReason, tc.reason) {
				t.Fatalf("narration = %+v, want fallback %q", n, tc.reason)
			}
		})
	}
}

func TestNarrate_GetProbeResultRejectsUncollectedProbes(t *testing.T) {
	ev := newEvidenceSet(lockIncident(t), incidentEvidence{})
	out := ev.run(probeToolCall("replication_slots"))
	if !strings.Contains(out, "not collected") {
		t.Fatalf("uncollected probe answer = %q", out)
	}
	out = ev.run(probeToolCall("DROP TABLE x"))
	if !strings.Contains(out, "not collected") {
		t.Fatalf("arbitrary probe name answer = %q", out)
	}
}

func TestNarrate_NonLockIncidentRunsItsFamilyWithoutHypotheses(t *testing.T) {
	fp := &fakeProbes{}
	eng := testEngine()
	eng.WithProbes(fp)
	inc := lockIncident(t)
	inc.SignalIDs = []string{"connections_high"}
	n := eng.narrate(context.Background(), inc)
	if strings.Contains(n.Text, "Likely") {
		t.Fatalf("connection pressure has no causal family in v1: %q", n.Text)
	}
	if !containsProbe(fp.calls(), probes.ConnectionSaturation) {
		t.Fatalf("probes run = %v", fp.calls())
	}
}

func TestNarrate_NoProbesKeepsM0Evidence(t *testing.T) {
	inc := lockIncident(t)
	ev := newEvidenceSet(inc, incidentEvidence{})
	for _, id := range ev.ids {
		if !strings.HasPrefix(id, "E") {
			t.Fatalf("evidence id %s without probes", id)
		}
	}
}

func probeToolCall(probe string) llm.ToolCall {
	return llm.ToolCall{ID: "c1", Name: "get_probe_result",
		Arguments: []byte(`{"probe":"` + probe + `"}`)}
}
