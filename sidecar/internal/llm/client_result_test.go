package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests here: ChatResult adds no shared state, and
// the client's concurrency (budget, throttle, breaker) has its own suite.

const (
	proseSystem = "You are pg_sage. Write a concise health briefing. Use markdown."
	jsonSystem  = "You are pg_sage. Respond with a JSON object."
	truncProse  = "## Summary\nThe index [critical: {a} and"
)

// finishServer answers every chat request with content and finish reason,
// and records each request body.
func finishServer(t *testing.T, content, finish string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": content},
				"finish_reason": finish,
			}},
			"usage": map[string]int{"total_tokens": 500},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func finishClient(url string, jsonMode bool, logFn func(string, string, ...any)) *Client {
	return New(&config.LLMConfig{
		Enabled: true, Endpoint: url, APIKey: "test-key", Model: "gpt-4o",
		TimeoutSeconds: 5, TokenBudgetDaily: 100000, CooldownSeconds: 1,
		JSONMode: jsonMode,
	}, logFn)
}

// Invalid input for the repair: truncated prose with brackets under a prose
// prompt comes back byte-identical, flagged as truncated.
func TestChatResult_TruncatedProseIsByteIdentical(t *testing.T) {
	srv, _ := finishServer(t, truncProse, "length")
	var logs []string
	logFn := func(_, msg string, args ...any) { logs = append(logs, fmt.Sprintf(msg, args...)) }
	got, err := finishClient(srv.URL, true, logFn).ChatResult(
		context.Background(), proseSystem, "data", 4096)
	if err != nil {
		t.Fatalf("ChatResult: %v", err)
	}
	want := Response{Content: truncProse, Tokens: 500, FinishReason: "length",
		Truncated: true, Repaired: false}
	if got != want {
		t.Fatalf("ChatResult = %+v, want %+v", got, want)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "finish_reason=length") {
		t.Fatalf("truncation not logged once with its reason: %q", logs)
	}
}

// Chat (the compatibility wrapper) no longer rewrites truncated prose.
func TestChat_TruncatedProseNotRewritten(t *testing.T) {
	srv, _ := finishServer(t, truncProse, "length")
	content, tokens, err := finishClient(srv.URL, false, noopLog).Chat(
		context.Background(), proseSystem, "data", 4096)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if content != truncProse || tokens != 500 {
		t.Fatalf("Chat = (%q, %d), want (%q, 500)", content, tokens, truncProse)
	}
}

// Happy path: a truncated object answer to a JSON prompt is repaired with
// its wrapper intact, and JSON mode still asks for json_object.
func TestChatResult_TruncatedObjectRepaired(t *testing.T) {
	for _, finish := range []string{"length", "max_tokens"} {
		t.Run(finish, func(t *testing.T) {
			srv, bodies := finishServer(t, `{"findings":[{"x":1},{"x":2`, finish)
			got, err := finishClient(srv.URL, true, noopLog).ChatResult(
				context.Background(), jsonSystem, "data", 4096)
			if err != nil {
				t.Fatalf("ChatResult: %v", err)
			}
			want := Response{Content: `{"findings":[{"x":1}]}`, Tokens: 500,
				FinishReason: finish, Truncated: true, Repaired: true}
			if got != want {
				t.Fatalf("ChatResult = %+v, want %+v", got, want)
			}
			rf, _ := (*bodies)[0]["response_format"].(map[string]any)
			if rf["type"] != "json_object" {
				t.Fatalf("response_format = %v, want json_object", (*bodies)[0]["response_format"])
			}
			var out struct {
				Findings []struct{ X int } `json:"findings"`
			}
			if err := json.Unmarshal([]byte(got.Content), &out); err != nil ||
				len(out.Findings) != 1 || out.Findings[0].X != 1 {
				t.Fatalf("repaired object does not decode: %v %+v", err, out)
			}
		})
	}
}

// A JSON answer whose prompt does not say "JSON" is still salvaged when the
// answer itself is a JSON document (the degradation-suite contract).
func TestChatResult_TruncatedArrayUnderPlainPrompt(t *testing.T) {
	srv, _ := finishServer(t, `[{"hint":"a"},{"hint":"b`, "length")
	got, err := finishClient(srv.URL, false, noopLog).ChatResult(
		context.Background(), "system", "user", 4096)
	if err != nil {
		t.Fatalf("ChatResult: %v", err)
	}
	if got.Content != `[{"hint":"a"}]` || !got.Truncated || !got.Repaired {
		t.Fatalf("ChatResult = %+v, want the first element salvaged", got)
	}
}

// Invalid input for the gate: prose that quotes a repairable JSON fragment
// is still prose, so it is not cut back to the fragment's first member.
func TestChatResult_ProseQuotingJSONNotRepaired(t *testing.T) {
	cut := "The model said {\"a\":1,\"b\":2 and then"
	if RepairTruncatedJSON(cut) == cut {
		t.Fatalf("probe %q must be repairable for this test to pin the gate", cut)
	}
	srv, _ := finishServer(t, cut, "length")
	got, err := finishClient(srv.URL, false, noopLog).ChatResult(
		context.Background(), proseSystem, "data", 4096)
	if err != nil {
		t.Fatalf("ChatResult: %v", err)
	}
	if got.Content != cut || !got.Truncated || got.Repaired {
		t.Fatalf("ChatResult = %+v, want the prose unchanged and truncated", got)
	}
}

// Boundary: nothing salvageable leaves the content unchanged, still flagged.
func TestChatResult_UnsalvageableJSONUnchanged(t *testing.T) {
	srv, _ := finishServer(t, `{"a"`, "length")
	got, err := finishClient(srv.URL, true, noopLog).ChatResult(
		context.Background(), jsonSystem, "data", 4096)
	if err != nil {
		t.Fatalf("ChatResult: %v", err)
	}
	if got.Content != `{"a"` || !got.Truncated || got.Repaired {
		t.Fatalf("ChatResult = %+v, want unchanged and truncated", got)
	}
}

// State: finish_reason=stop is not truncated and is never repaired, even
// when the content looks cut.
func TestChatResult_StopIsNotTruncated(t *testing.T) {
	srv, _ := finishServer(t, `{"findings":[{"x":1},{"x":2`, "stop")
	var logs []string
	logFn := func(_, msg string, args ...any) { logs = append(logs, fmt.Sprintf(msg, args...)) }
	got, err := finishClient(srv.URL, true, logFn).ChatResult(
		context.Background(), jsonSystem, "data", 4096)
	if err != nil {
		t.Fatalf("ChatResult: %v", err)
	}
	want := Response{Content: `{"findings":[{"x":1},{"x":2`, Tokens: 500,
		FinishReason: "stop"}
	if got != want {
		t.Fatalf("ChatResult = %+v, want %+v", got, want)
	}
	if len(logs) != 0 {
		t.Fatalf("a complete answer logged truncation: %q", logs)
	}
}

// Error propagation: a blank truncated answer is ErrEmptyResponse, with the
// tokens still charged and the finish reason reported.
func TestChatResult_EmptyTruncatedIsError(t *testing.T) {
	srv, _ := finishServer(t, "  ", "length")
	got, err := finishClient(srv.URL, false, noopLog).ChatResult(
		context.Background(), proseSystem, "data", 4096)
	if !errors.Is(err, ErrEmptyResponse) {
		t.Fatalf("err = %v, want ErrEmptyResponse", err)
	}
	if got.Tokens != 500 || got.Content != "" || got.FinishReason != "length" ||
		!got.Truncated {
		t.Fatalf("ChatResult = %+v, want tokens 500, finish length, truncated", got)
	}
}

// Nil/zero: a disabled client returns an error and a zero Response.
func TestChatResult_DisabledClient(t *testing.T) {
	got, err := New(nil, noopLog).ChatResult(context.Background(), "s", "u", 10)
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("err = %v, want LLM not enabled", err)
	}
	if got != (Response{}) {
		t.Fatalf("ChatResult = %+v, want zero Response", got)
	}
}
