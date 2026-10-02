package sre

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Fixtures for the model turn (Sage SRE M3). fakeModel is an
// OpenAI-compatible chat completions server answering from a script; it
// records every request body so tests can assert what the model saw.
// No test reaches a live provider.

type fakeReply func(w http.ResponseWriter, body string)

type fakeModel struct {
	srv    *httptest.Server
	mu     sync.Mutex
	script []fakeReply
	bodies []string
}

// newFakeModel answers the n-th request with the n-th reply; requests
// beyond the script get HTTP 500 (and still count as calls).
func newFakeModel(t *testing.T, script ...fakeReply) *fakeModel {
	t.Helper()
	f := &fakeModel{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		n := len(f.bodies)
		f.bodies = append(f.bodies, string(raw))
		f.mu.Unlock()
		if n >= len(f.script) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.script[n](w, string(raw))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeModel) client() *llm.Client { return llmClient(f.srv.URL, true) }

func (f *fakeModel) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeModel) body(t *testing.T, i int) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("request %d not made (%d requests)", i, len(f.bodies))
	}
	return f.bodies[i]
}

func writeCompletion(w http.ResponseWriter, msg map[string]any) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		"usage":   map[string]int{"total_tokens": 300},
	})
}

// toolReply answers with a submit_review tool call built from the request.
func toolReply(build func(body string) string) fakeReply {
	return callTool(reviewToolName, build)
}

// callTool answers with a call of any (possibly undeclared) tool.
func callTool(name string, build func(body string) string) fakeReply {
	return func(w http.ResponseWriter, body string) {
		writeCompletion(w, map[string]any{"role": "assistant", "content": "",
			"tool_calls": []map[string]any{{"id": "call_1", "type": "function",
				"function": map[string]any{"name": name, "arguments": build(body)}}}})
	}
}

// contentReply answers with plain message content built from the request.
func contentReply(build func(body string) string) fakeReply {
	return func(w http.ResponseWriter, body string) {
		writeCompletion(w, map[string]any{"role": "assistant", "content": build(body)})
	}
}

func statusReply(code int) fakeReply {
	return func(w http.ResponseWriter, _ string) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":{"message":"tools are not supported"}}`))
	}
}

func slowReply(d time.Duration, next fakeReply) fakeReply {
	return func(w http.ResponseWriter, body string) {
		time.Sleep(d)
		next(w, body)
	}
}

func fixed(s string) func(string) string { return func(string) string { return s } }

// aliasOf finds the evidence alias the prompt gave a probe result with a
// status, e.g. "E1" for "E1 [lock_graph ok]".
func aliasOf(t *testing.T, body string, probe probes.ID, status string) string {
	t.Helper()
	re := regexp.MustCompile(`(E\d+) \[` + regexp.QuoteMeta(string(probe)) + " " +
		regexp.QuoteMeta(status) + `\]`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Errorf("prompt has no %s %s evidence: %s", probe, status, body)
		return "E0"
	}
	return m[1]
}

type wireClaim struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type wireReview struct {
	Ranking   []string       `json:"ranking"`
	NextProbe map[string]any `json:"next_probe,omitempty"`
	Claims    []wireClaim    `json:"claims"`
}

func (r wireReview) json() string {
	if r.Claims == nil {
		r.Claims = []wireClaim{}
	}
	raw, _ := json.Marshal(r)
	return string(raw)
}

func nextProbe(id string, args map[string]any, rationale string) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	return map[string]any{"probe": id, "args": args, "rationale": rationale}
}

// idleClaim is grounded in the idle-chain lock graph facts (pid 4242,
// 90 s open, 2 blocked sessions).
func idleClaim(alias string) wireClaim {
	return wireClaim{Text: "pid 4242 is idle in transaction, has held its transaction " +
		"open for 90 s and blocks 2 sessions.", EvidenceIDs: []string{alias}}
}

// validIdleReview agrees with the idle-chain graph and narrates one
// grounded claim.
func validIdleReview(t *testing.T) func(string) string {
	return func(body string) string {
		return wireReview{Ranking: []string{"idle_in_tx_holder", "ddl_lock_queue"},
			Claims: []wireClaim{idleClaim(aliasOf(t, body, probes.LockGraph, "ok"))}}.json()
	}
}

// fixtureEvidence stores results as evidence the way CommitStep does:
// canonical payload, its hash and a fresh id, in the given order. A
// result without a time is observed i seconds after the first dated one
// (or a fixed time), so one investigation's evidence is seconds apart.
func fixtureEvidence(t *testing.T, results ...probes.Result) []Evidence {
	t.Helper()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, res := range results {
		if !res.ObservedAt.IsZero() {
			base = res.ObservedAt
			break
		}
	}
	out := make([]Evidence, 0, len(results))
	for i, res := range results {
		if res.ObservedAt.IsZero() {
			res.ObservedAt = base.Add(time.Duration(i) * time.Second)
		}
		payload, err := canonicalPayload(res)
		if err != nil {
			t.Fatalf("payload: %v", err)
		}
		sum := sha256.Sum256(payload)
		out = append(out, Evidence{ID: NewUUID(), StepKey: "step-1",
			ProbeID: string(res.ProbeID), ProbeVersion: res.Version,
			CapabilityState: capabilityState(res.Status), ObservedAt: res.ObservedAt,
			Payload: payload, SHA256: sum[:]})
	}
	return out
}

// idleChainFixture is the conclusive idle-in-transaction chain as stored
// evidence (E1 lock_graph, E2 prepared_xacts, E3 sage_actions) and its
// deterministic diagnosis.
func idleChainFixture(t *testing.T) (Investigation, []Evidence, causal.Diagnosis) {
	t.Helper()
	r := idleChainRunner()
	ctx := context.Background()
	ev := fixtureEvidence(t, r.Run(ctx, probes.LockGraph, probes.Args{}),
		r.Run(ctx, probes.PreparedXacts, probes.Args{}),
		rows(probes.SageActions))
	inv := Investigation{TriggerKind: TriggerLock, Subject: "incident 1"}
	return inv, ev, diagnoseEvidence(t, inv, ev)
}

// unknownLockFixture is an inconclusive lock investigation: the lock
// graph probe timed out (E1), so no root blocker is known.
func unknownLockFixture(t *testing.T) (Investigation, []Evidence, causal.Diagnosis) {
	t.Helper()
	ev := fixtureEvidence(t, lockGraphTimeout(), rows(probes.PreparedXacts),
		rows(probes.SageActions))
	inv := Investigation{TriggerKind: TriggerLock, Subject: "incident 2"}
	return inv, ev, diagnoseEvidence(t, inv, ev)
}

func lockGraphTimeout() probes.Result {
	return probes.Result{ProbeID: probes.LockGraph, Version: "v1",
		Status: probes.StatusError, Reason: "statement_timeout"}
}

func diagnoseEvidence(t *testing.T, inv Investigation, ev []Evidence) causal.Diagnosis {
	t.Helper()
	obs, err := observations(ev)
	if err != nil {
		t.Fatalf("observations: %v", err)
	}
	return diagnose(inv, obs)
}

// rejection asserts a rejection with the reason.
func rejection(t *testing.T, rej *ModelRejection, reason string) {
	t.Helper()
	if rej == nil {
		t.Fatalf("no rejection, want %q", reason)
	}
	if rej.Reason != reason {
		t.Fatalf("rejection = %q (%s), want %q", rej.Reason, rej.Detail, reason)
	}
}
