package rca

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Sage SRE M0 incident narration: the LLM may only narrate evidence the
// engine already holds, through ChatWithTools, within a strict budget.
// Every failure degrades to the deterministic narrative.

type narrServer struct {
	srv    *httptest.Server
	calls  atomic.Int32
	mu     sync.Mutex
	bodies []map[string]any
	reply  func(n int, w http.ResponseWriter)
}

func newNarrServer(t *testing.T, reply func(n int, w http.ResponseWriter)) *narrServer {
	t.Helper()
	s := &narrServer{reply: reply}
	s.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			n := int(s.calls.Add(1))
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			s.mu.Lock()
			s.bodies = append(s.bodies, body)
			s.mu.Unlock()
			s.reply(n, w)
		}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *narrServer) body(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[i]
}

func writeCompletion(w http.ResponseWriter, content string, calls []map[string]any) {
	msg := map[string]any{"role": "assistant", "content": content}
	if calls != nil {
		msg["tool_calls"] = calls
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
		"usage":   map[string]int{"total_tokens": 100},
	})
}

func evidenceCall(id string) []map[string]any {
	return []map[string]any{{"id": "call_" + id, "type": "function",
		"function": map[string]any{"name": "get_evidence",
			"arguments": `{"id":"` + id + `"}`}}}
}

// scripted replies: turn 1 asks for E2, turn 2 answers with final.
func twoTurn(final string) func(int, http.ResponseWriter) {
	return func(n int, w http.ResponseWriter) {
		if n == 1 {
			writeCompletion(w, "", evidenceCall("E2"))
			return
		}
		writeCompletion(w, final, nil)
	}
}

func narrEngine(url string, enabled bool) *Engine {
	eng := testEngine()
	eng.cfg.NarrationEnabled = enabled
	eng.WithLLM(llm.New(&config.LLMConfig{
		Enabled: true, Endpoint: url, APIKey: "k", Model: "m",
		TimeoutSeconds: 10, TokenBudgetDaily: 1000000,
	}, noopTestLog))
	return eng
}

// lockIncident is a lock_contention incident whose E2 evidence names
// pid 4242 blocking 3 sessions.
func lockIncident(t *testing.T) Incident {
	t.Helper()
	eng := testEngine()
	eng.WithDatabaseName("orders_db")
	incs := eng.ObserveLockChains(context.Background(),
		[]analyzer.Finding{chainFinding(4242, 3, 2, "idle in transaction")})
	if len(incs) != 1 {
		t.Fatalf("setup: %d incidents", len(incs))
	}
	return incs[0]
}

const groundedFinal = `{"summary": "Session 4242 has been idle in ` +
	`transaction and blocks 3 sessions; end it or wait.", ` +
	`"evidence_ids": ["E2"]}`

func assertDeterministic(
	t *testing.T, n Narration, inc Incident, reason string,
) {
	t.Helper()
	if n.Source != NarrationDeterministic {
		t.Fatalf("source = %q, want deterministic (text %q)", n.Source, n.Text)
	}
	if !strings.Contains(n.FallbackReason, reason) {
		t.Fatalf("fallback reason = %q, want it to mention %q",
			n.FallbackReason, reason)
	}
	if n.Text == "" || n.Text != DeterministicNarration(inc).Text {
		t.Fatalf("fallback text %q is not the deterministic narrative", n.Text)
	}
}

func TestDeterministicNarration_CitesEvidence(t *testing.T) {
	inc := lockIncident(t)
	n := DeterministicNarration(inc)
	if n.Source != NarrationDeterministic || n.FallbackReason != "" {
		t.Fatalf("narration = %+v", n)
	}
	for _, want := range []string{"4242", "E1", "E2", "idle in transaction"} {
		if !strings.Contains(n.Text, want) {
			t.Errorf("deterministic narrative %q missing %q", n.Text, want)
		}
	}
	if strings.Contains(n.Text, "hunter2") {
		t.Fatal("deterministic narrative leaked query text")
	}
	empty := DeterministicNarration(Incident{})
	if empty.Text == "" || empty.Source != NarrationDeterministic {
		t.Fatalf("empty incident narration = %+v", empty)
	}
}

func TestNarrate_DisabledIsKillSwitch(t *testing.T) {
	s := newNarrServer(t, twoTurn(groundedFinal))
	eng := narrEngine(s.srv.URL, false)
	inc := lockIncident(t)
	n := eng.narrate(context.Background(), inc)
	assertDeterministic(t, n, inc, "disabled")
	if s.calls.Load() != 0 {
		t.Fatal("disabled narration called the LLM")
	}
}

func TestNarrate_NoLLMClient(t *testing.T) {
	eng := testEngine()
	eng.cfg.NarrationEnabled = true
	inc := lockIncident(t)
	assertDeterministic(t, eng.narrate(context.Background(), inc), inc,
		"llm unavailable")
}

func TestNarrate_ToolLoopHappyPath(t *testing.T) {
	s := newNarrServer(t, twoTurn(groundedFinal))
	eng := narrEngine(s.srv.URL, true)
	n := eng.narrate(context.Background(), lockIncident(t))
	if n.Source != NarrationLLM || n.FallbackReason != "" {
		t.Fatalf("narration = %+v", n)
	}
	if !strings.Contains(n.Text, "4242") || len(n.Citations) != 1 ||
		n.Citations[0] != "E2" {
		t.Fatalf("narration = %+v", n)
	}
	if s.calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", s.calls.Load())
	}
	if s.body(0)["tool_choice"] != "auto" || s.body(1)["tool_choice"] != "none" {
		t.Fatalf("tool_choice turn1=%v turn2=%v", s.body(0)["tool_choice"],
			s.body(1)["tool_choice"])
	}
	msgs := s.body(1)["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	content, _ := last["content"].(string)
	if last["role"] != "tool" || !strings.Contains(content, "<data") ||
		!strings.Contains(content, "pid=4242") {
		t.Fatalf("tool result not returned as fenced evidence: %v", last)
	}
	sys := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, llm.UntrustedDataRule) {
		t.Fatal("system prompt lacks the untrusted-data rule")
	}
}

func TestNarrate_DirectAnswerWithoutTools(t *testing.T) {
	s := newNarrServer(t, func(_ int, w http.ResponseWriter) {
		writeCompletion(w, groundedFinal, nil)
	})
	n := narrEngine(s.srv.URL, true).narrate(context.Background(), lockIncident(t))
	if n.Source != NarrationLLM || s.calls.Load() != 1 {
		t.Fatalf("narration = %+v after %d calls", n, s.calls.Load())
	}
}

func TestNarrate_MarkdownWrappedJSONAccepted(t *testing.T) {
	s := newNarrServer(t, twoTurn("```json\n"+groundedFinal+"\n```"))
	n := narrEngine(s.srv.URL, true).narrate(context.Background(), lockIncident(t))
	if n.Source != NarrationLLM || n.Citations[0] != "E2" {
		t.Fatalf("fenced JSON not recovered: %+v", n)
	}
}

func TestNarrate_DegradesOnBadModelOutput(t *testing.T) {
	cases := map[string]struct{ final, reason string }{
		"malformed json": {`{"summary": "x", "evidence_ids": [`, "malformed"},
		"prose":          {`The session is blocking others.`, "malformed"},
		"empty summary":  {`{"summary": " ", "evidence_ids": ["E2"]}`, "summary"},
		"no citations":   {`{"summary": "Blocking.", "evidence_ids": []}`, "cite"},
		"unknown id":     {`{"summary": "Blocking.", "evidence_ids": ["E9"]}`, "unknown evidence"},
		"invented number": {`{"summary": "Session 777 blocks.", ` +
			`"evidence_ids": ["E2"]}`, "ungrounded number"},
		"markdown garbage": {"```json\n{summary: nope}\n```", "malformed"},
		"too long": {`{"summary": "` + strings.Repeat("a", maxNarrativeRunes+1) +
			`", "evidence_ids": ["E2"]}`, "summary"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newNarrServer(t, twoTurn(tc.final))
			eng := narrEngine(s.srv.URL, true)
			inc := lockIncident(t)
			assertDeterministic(t, eng.narrate(context.Background(), inc),
				inc, tc.reason)
		})
	}
}

func TestNarrate_EmptyResponse(t *testing.T) {
	s := newNarrServer(t, twoTurn(""))
	inc := lockIncident(t)
	assertDeterministic(t, narrEngine(s.srv.URL, true).narrate(
		context.Background(), inc), inc, "empty")
}

func TestNarrate_RateLimited(t *testing.T) {
	s := newNarrServer(t, func(_ int, w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	start := time.Now()
	inc := lockIncident(t)
	n := narrEngine(s.srv.URL, true).narrate(context.Background(), inc)
	assertDeterministic(t, n, inc, "rate limited")
	if time.Since(start) > 2*time.Second {
		t.Fatalf("rate-limited narration took %s", time.Since(start))
	}
}

func TestNarrate_TimeoutDegrades(t *testing.T) {
	block := make(chan struct{})
	s := newNarrServer(t, func(int, http.ResponseWriter) { <-block })
	t.Cleanup(func() { close(block) }) // before srv.Close (LIFO)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	inc := lockIncident(t)
	n := narrEngine(s.srv.URL, true).narrate(ctx, inc)
	assertDeterministic(t, n, inc, "timeout")
	if time.Since(start) > 2*time.Second {
		t.Fatalf("narration ignored its deadline: %s", time.Since(start))
	}
}

func TestNarrate_ToolLoopBounded(t *testing.T) {
	s := newNarrServer(t, func(_ int, w http.ResponseWriter) {
		writeCompletion(w, "", evidenceCall("E1"))
	})
	inc := lockIncident(t)
	n := narrEngine(s.srv.URL, true).narrate(context.Background(), inc)
	assertDeterministic(t, n, inc, "turn limit")
	if got := s.calls.Load(); got != narrationMaxTurns {
		t.Fatalf("provider calls = %d, want %d", got, narrationMaxTurns)
	}
}

func TestNarrate_InputBudgetRefusedBeforeCall(t *testing.T) {
	s := newNarrServer(t, twoTurn(groundedFinal))
	inc := lockIncident(t)
	inc.RootCause = strings.Repeat("x", 4*narrationInputBudgetTokens+100)
	n := narrEngine(s.srv.URL, true).narrate(context.Background(), inc)
	assertDeterministic(t, n, inc, "budget")
	if s.calls.Load() != 0 {
		t.Fatal("over-budget narration still called the provider")
	}
}

func TestNarrate_KillSwitchMidFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s := newNarrServer(t, func(int, http.ResponseWriter) {
		once.Do(func() { close(started) })
		<-release
	})
	t.Cleanup(func() { close(release) }) // before srv.Close (LIFO)
	eng := narrEngine(s.srv.URL, true)
	client := eng.llmClient
	out := make(chan Narration, 1)
	go func() { out <- eng.narrate(context.Background(), lockIncident(t)) }()
	<-started
	client.Reconfigure(&config.LLMConfig{Enabled: false})
	select {
	case n := <-out:
		if n.Source != NarrationDeterministic {
			t.Fatalf("narration after kill switch = %+v", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("kill switch did not stop the narration")
	}
}

// The LLM call never holds e.mu: the engine stays readable while a
// narration is in flight.
func TestNarrate_DoesNotHoldEngineMutex(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s := newNarrServer(t, func(_ int, w http.ResponseWriter) {
		once.Do(func() { close(started) })
		<-release
		writeCompletion(w, groundedFinal, nil)
	})
	eng := narrEngine(s.srv.URL, true)
	done := make(chan struct{})
	go func() {
		eng.narrate(context.Background(), lockIncident(t))
		close(done)
	}()
	<-started
	read := make(chan struct{})
	go func() {
		eng.ActiveIncidents()
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(time.Second):
		t.Fatal("ActiveIncidents blocked while a narration was in flight")
	}
	close(release)
	<-done
}

func TestNarrate_ConcurrentCallsIndependent(t *testing.T) {
	s := newNarrServer(t, func(_ int, w http.ResponseWriter) {
		writeCompletion(w, groundedFinal, nil)
	})
	eng := narrEngine(s.srv.URL, true)
	inc := lockIncident(t)
	var wg sync.WaitGroup
	results := make([]Narration, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := inc
			c.ID = inc.ID + string(rune('a'+i)) // distinct work items
			results[i] = eng.narrate(context.Background(), c)
		}(i)
	}
	wg.Wait()
	for i, n := range results {
		if n.Source != NarrationLLM || n.Citations[0] != "E2" {
			t.Fatalf("result %d = %+v", i, n)
		}
	}
}

func pending(inc Incident, typ string) pendingEvent {
	var ev notify.Event
	switch typ {
	case "incident_resolved":
		ev = notify.IncidentResolvedEvent(incidentInfo(&inc))
	default:
		ev = notify.IncidentDetectedEvent(incidentInfo(&inc))
	}
	return pendingEvent{event: ev, incident: inc}
}

func TestDecorateEvents_AddsLabeledNarrative(t *testing.T) {
	s := newNarrServer(t, twoTurn(groundedFinal))
	eng := narrEngine(s.srv.URL, true)
	inc := lockIncident(t)
	out := eng.decorateEvents(context.Background(), []pendingEvent{
		pending(inc, "incident_detected"), pending(inc, "incident_resolved"),
	})
	if len(out) != 2 {
		t.Fatalf("events = %d, want 2", len(out))
	}
	det := out[0]
	if det.Data["narrative_source"] != NarrationLLM ||
		!strings.Contains(det.Data["narrative"].(string), "4242") {
		t.Fatalf("detected data = %v", det.Data)
	}
	if !strings.Contains(det.Body, "Summary (LLM, cites E2)") {
		t.Fatalf("detected body lacks the labeled narrative: %q", det.Body)
	}
	if _, ok := out[1].Data["narrative"]; ok {
		t.Fatal("resolved events must not be narrated")
	}
}

func TestDecorateEvents_DeterministicWhenDisabled(t *testing.T) {
	eng := testEngine()
	inc := lockIncident(t)
	out := eng.decorateEvents(context.Background(),
		[]pendingEvent{pending(inc, "incident_detected")})
	if out[0].Data["narrative_source"] != NarrationDeterministic ||
		!strings.Contains(out[0].Body, "Summary (deterministic)") {
		t.Fatalf("event = %+v", out[0])
	}
}

// At most narrationBatchLimit LLM narrations run per persistence cycle;
// the rest fall back so a burst of incidents cannot stall the cycle.
func TestDecorateEvents_BatchLimit(t *testing.T) {
	s := newNarrServer(t, func(_ int, w http.ResponseWriter) {
		writeCompletion(w, groundedFinal, nil)
	})
	eng := narrEngine(s.srv.URL, true)
	inc := lockIncident(t)
	var batch []pendingEvent
	for i := 0; i < narrationBatchLimit+2; i++ {
		c := inc
		c.ID = inc.ID + string(rune('a'+i))
		batch = append(batch, pending(c, "incident_detected"))
	}
	out := eng.decorateEvents(context.Background(), batch)
	llmCount := 0
	for _, ev := range out {
		if ev.Data["narrative_source"] == NarrationLLM {
			llmCount++
		}
	}
	if llmCount != narrationBatchLimit || int(s.calls.Load()) != narrationBatchLimit {
		t.Fatalf("llm narrations = %d, calls = %d, want %d", llmCount,
			s.calls.Load(), narrationBatchLimit)
	}
	last := out[len(out)-1]
	if last.Data["narrative_fallback_reason"] == nil ||
		!strings.Contains(last.Data["narrative_fallback_reason"].(string), "limit") {
		t.Fatalf("over-limit event = %v", last.Data)
	}
}
