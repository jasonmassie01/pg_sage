package sre

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// One model turn inside an investigation: durable reservation, inflight
// mark, a ChatWithTools call bounded by the reservation (the per-call
// Budgeter), and settlement. Never calls a live provider. LLM negatives:
// malformed JSON, markdown-wrapped JSON, empty, timeout, 429.

type mockLLM struct {
	srv   *httptest.Server
	calls atomic.Int32
}

func newMockLLM(t *testing.T, reply func(w http.ResponseWriter)) *mockLLM {
	t.Helper()
	m := &mockLLM{}
	m.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			m.calls.Add(1)
			reply(w)
		}))
	t.Cleanup(m.srv.Close)
	return m
}

func completion(content string, calls []map[string]any, total int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		msg := map[string]any{"role": "assistant", "content": content}
		if calls != nil {
			msg["tool_calls"] = calls
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}},
			"usage":   map[string]int{"total_tokens": total},
		})
	}
}

func llmClient(url string, enabled bool) *llm.Client {
	return llm.New(&config.LLMConfig{Enabled: enabled, Endpoint: url,
		APIKey: "k", Model: "m", TimeoutSeconds: 5, TokenBudgetDaily: 1_000_000},
		func(string, string, ...any) {})
}

var probeTools = []llm.ToolSpec{{Name: "get_evidence",
	Description: "Read one evidence item.",
	Parameters: json.RawMessage(`{"type":"object","properties":` +
		`{"id":{"type":"string"}},"required":["id"]}`)}}

func turn(key string) ModelTurn {
	return ModelTurn{RequestKey: key, Input: 4000, Output: 1000, Tools: probeTools,
		Messages: []llm.Message{{Role: "system", Content: "Narrate evidence."},
			{Role: "user", Content: "Incident on pid 10."}}}
}

func TestRunModelTurn_SuccessSettlesActualUsage(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 60")
	m := newMockLLM(t, completion(`{"summary":"ok"}`, nil, 120))
	out, err := RunModelTurn(ctx, st, lease, llmClient(m.srv.URL, true), turn("t1"))
	if err != nil || out.Result.Content != `{"summary":"ok"}` {
		t.Fatalf("turn = %+v (%v)", out, err)
	}
	if out.Reservation.State != ReservationSettled || out.Reservation.InputUsed+
		out.Reservation.OutputUsed != 120 {
		t.Fatalf("reservation = %+v, want settled with 120 tokens", out.Reservation)
	}
}

func TestRunModelTurn_MarkdownWrappedToolArgsAccepted(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 61")
	m := newMockLLM(t, completion("", []map[string]any{{"id": "c1",
		"type": "function", "function": map[string]any{"name": "get_evidence",
			"arguments": "```json\n{\"id\":\"P1\"}\n```"}}}, 90))
	out, err := RunModelTurn(ctx, st, lease, llmClient(m.srv.URL, true), turn("t1"))
	if err != nil || len(out.Result.ToolCalls) != 1 ||
		string(out.Result.ToolCalls[0].Arguments) != `{"id":"P1"}` {
		t.Fatalf("turn = %+v (%v)", out, err)
	}
	if out.Reservation.State != ReservationSettled {
		t.Fatalf("reservation = %+v", out.Reservation)
	}
}

func TestRunModelTurn_Negatives(t *testing.T) {
	cases := map[string]struct {
		reply     func(http.ResponseWriter)
		wantErr   error
		wantState ReservationState
	}{
		"malformed body": {func(w http.ResponseWriter) { _, _ = w.Write([]byte("{nope")) },
			nil, ReservationUnknown},
		"empty response": {completion("", nil, 40), llm.ErrEmptyResponse,
			ReservationSettled},
		"rate limited": {func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusTooManyRequests)
		}, llm.ErrRateLimited, ReservationSettled},
		"timeout": {func(w http.ResponseWriter) {
			time.Sleep(400 * time.Millisecond)
			completion("late", nil, 10)(w)
		}, context.DeadlineExceeded, ReservationUnknown},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			st, _, ctx := liveStore(t, budgetLimits())
			lease := claimed(t, st, "pid 62 "+name)
			m := newMockLLM(t, c.reply)
			tr := turn("t1")
			tr.Options.Timeout = 100 * time.Millisecond
			out, err := RunModelTurn(ctx, st, lease, llmClient(m.srv.URL, true), tr)
			if err == nil || (c.wantErr != nil && !errors.Is(err, c.wantErr)) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if out.Reservation.State != c.wantState {
				t.Fatalf("reservation = %+v, want %s", out.Reservation, c.wantState)
			}
			if m.calls.Load() != 1 {
				t.Fatalf("provider calls = %d, want exactly 1 (no retry)", m.calls.Load())
			}
		})
	}
}

func TestRunModelTurn_NotDispatchedSettlesZero(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 63")
	m := newMockLLM(t, completion("x", nil, 10))
	out, err := RunModelTurn(ctx, st, lease, llmClient(m.srv.URL, false), turn("off"))
	if !errors.Is(err, llm.ErrLLMDisabled) || m.calls.Load() != 0 {
		t.Fatalf("disabled client: err=%v calls=%d", err, m.calls.Load())
	}
	if out.Reservation.State != ReservationSettled || out.Reservation.InputUsed != 0 {
		t.Fatalf("reservation = %+v, want settled at zero", out.Reservation)
	}
	small := turn("small")
	small.Input, small.Output = 5, 5
	out, err = RunModelTurn(ctx, st, lease, llmClient(m.srv.URL, true), small)
	if !errors.Is(err, llm.ErrBudgetExhausted) || m.calls.Load() != 0 {
		t.Fatalf("over-reservation call: err=%v calls=%d", err, m.calls.Load())
	}
	if out.Reservation.State != ReservationSettled || out.Reservation.InputUsed != 0 {
		t.Fatalf("reservation = %+v, want settled at zero", out.Reservation)
	}
}

func TestRunModelTurn_TurnCapStopsBeforeProvider(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 64")
	m := newMockLLM(t, completion(`{"a":1}`, nil, 10))
	client := llmClient(m.srv.URL, true)
	for i, key := range []string{"t1", "t2"} {
		if _, err := RunModelTurn(ctx, st, lease, client, turn(key)); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}
	if _, err := RunModelTurn(ctx, st, lease, client, turn("t3")); !errors.Is(err,
		ErrBudgetExhausted) {
		t.Fatalf("third turn = %v, want ErrBudgetExhausted", err)
	}
	if m.calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2", m.calls.Load())
	}
}

func TestRunModelTurn_NilClient(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	lease := claimed(t, st, "pid 65")
	if _, err := RunModelTurn(ctx, st, lease, nil, turn("nil")); !errors.Is(err,
		llm.ErrLLMDisabled) {
		t.Fatalf("nil client = %v", err)
	}
	if inv, _ := st.Get(ctx, lease.Scope, lease.InvestigationID); inv.ModelTurns != 0 {
		t.Fatalf("a nil client consumed %d turns", inv.ModelTurns)
	}
}
