package srebench

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// The fake adversarial model of the LLM-on arm (CI default). It is an
// in-process OpenAI-compatible chat completions server that answers the
// investigator's model contract against the graph but in a valid shape:
// it ranks the graph's last open hypothesis first, asks for a next probe
// whenever one is offered, and narrates claims citing the prompt's real
// evidence ids. On a deterministic subset of calls (seeded by the
// scenario id) it answers with a failure mode the investigator must
// handle. It measures that the model plumbing cannot lower Safe Pass or
// change a conclusive root; it says nothing about a real model's quality.

// FakeModelName is the model name the fake reports (not a thinking model).
const FakeModelName = "pgincidentbench-fake"

// FakeMode is how the fake answers one call.
type FakeMode string

// Fake reply modes.
const (
	FakeAdversarial FakeMode = "adversarial"
	FakeFenced      FakeMode = "fenced_json"
	FakeUnknownNode FakeMode = "unknown_node"
	FakeUngrounded  FakeMode = "ungrounded_number"
	FakeRateLimited FakeMode = "rate_limited"
)

// fakeModes weights the modes: six adversarial calls in ten, one of each
// failure mode.
var fakeModes = []FakeMode{FakeAdversarial, FakeAdversarial, FakeAdversarial,
	FakeAdversarial, FakeAdversarial, FakeAdversarial, FakeFenced, FakeUnknownNode,
	FakeUngrounded, FakeRateLimited}

func seedOf(scenarioID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(scenarioID))
	return h.Sum64()
}

// FakeModeFor is the mode of a scenario's call (0-based), deterministic.
func FakeModeFor(scenarioID string, call int) FakeMode {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d:%d", seedOf(scenarioID), call)
	return fakeModes[h.Sum64()%uint64(len(fakeModes))]
}

// FakeModel is one scenario run's fake model server.
type FakeModel struct {
	scenario string
	srv      *httptest.Server
	mu       sync.Mutex
	calls    int
}

// NewFakeModel starts the fake for one scenario run.
func NewFakeModel(scenarioID string) *FakeModel {
	f := &FakeModel{scenario: scenarioID}
	f.srv = httptest.NewServer(f)
	return f
}

// URL is the fake's OpenAI-compatible endpoint.
func (f *FakeModel) URL() string { return f.srv.URL }

// Close stops the fake.
func (f *FakeModel) Close() { f.srv.Close() }

// Calls counts the requests the fake received.
func (f *FakeModel) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// skip advances the call counter without a request (tests).
func (f *FakeModel) skip() { f.next() }

func (f *FakeModel) next() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.calls - 1
}

type fakeRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []json.RawMessage `json:"tools"`
}

// ServeHTTP answers one chat completion in the call's mode.
func (f *FakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := f.next()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req fakeRequest
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	mode := FakeModeFor(f.scenario, call)
	if mode == FakeRateLimited {
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
		return
	}
	var user string
	for _, m := range req.Messages {
		if m.Role == "user" && user == "" {
			user = m.Content
		}
	}
	review := adversarialReview(parseFakePrompt(user), seedOf(f.scenario)+uint64(call))
	writeFakeReply(w, mode.apply(review), len(req.Tools) > 0, mode == FakeFenced)
}

// apply turns the adversarial review into the mode's reply.
func (m FakeMode) apply(r fakeReview) fakeReview {
	switch m {
	case FakeUnknownNode:
		r.Ranking = append([]string{"not_a_graph_node"}, r.Ranking...)
	case FakeUngrounded:
		alias := "E1"
		if len(r.Claims) > 0 {
			alias = r.Claims[0].EvidenceIDs[0]
		}
		r.Claims = append(r.Claims, fakeClaim{Text: "It blocks 987654 sessions.",
			EvidenceIDs: []string{alias}})
	}
	return r
}

// writeFakeReply answers as a submit_review tool call when tools were
// offered, else as content (fenced when asked).
func writeFakeReply(w http.ResponseWriter, r fakeReview, tools, fenced bool) {
	body, _ := json.Marshal(r)
	msg := map[string]any{"role": "assistant", "content": string(body)}
	if fenced {
		msg["content"] = "```json\n" + string(body) + "\n```"
	} else if tools {
		msg["content"] = ""
		msg["tool_calls"] = []map[string]any{{"id": "call_1", "type": "function",
			"function": map[string]any{"name": "submit_review",
				"arguments": string(body)}}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		"usage": map[string]int{"prompt_tokens": 1500, "completion_tokens": 300,
			"total_tokens": 1800}})
}
