package llm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Current OpenAI models (gpt-5*, gpt-6*, o-series) reject max_tokens with
// HTTP 400 and ask for max_completion_tokens. In llm.token_parameter=auto
// the client retries such a 400 once with max_completion_tokens and
// remembers the shape per (endpoint, model) for the whole process.

func TestChat_AutoAdaptsToMaxCompletionTokens(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.maxTokensErr = openAIMaxTokensErr })
	logs := &capLog{}
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", logs.log)
	content, tokens, err := c.Chat(context.Background(), "sys", "user", 500)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if content != "ok" || tokens != 120+64 {
		t.Fatalf("content=%q tokens=%d, want ok/184", content, tokens)
	}
	if f.calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2 (rejected, then adapted)", f.calls.Load())
	}
	first, second := f.request(t, 0), f.request(t, 1)
	sent := numField(t, first, "max_tokens")
	if _, ok := first["max_completion_tokens"]; ok {
		t.Fatalf("first request already adapted: %v", first)
	}
	if _, ok := second["max_tokens"]; ok {
		t.Fatalf("retry still sends max_tokens: %v", second)
	}
	if got := numField(t, second, "max_completion_tokens"); got != sent {
		t.Fatalf("max_completion_tokens = %d, want the same cap %d", got, sent)
	}
	if _, ok := second["reasoning_effort"]; ok {
		t.Fatal("plain chat must never send reasoning_effort")
	}
	if c.TokensUsedToday() != 184 {
		t.Fatalf("daily budget charged %d, want 184 (once)", c.TokensUsedToday())
	}
	if failureCount(c) != 0 || c.IsCircuitOpen() {
		t.Fatalf("adaptation counted as a provider failure (%d)", failureCount(c))
	}
	if n := logs.count("max_completion_tokens", "gpt-6-luna"); n != 1 {
		t.Fatalf("adaptation logged %d times, want 1: %v", n, logs.lines)
	}
}

func TestChat_AdaptationRememberedAcrossCallsAndClients(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.maxTokensErr = openAIMaxTokensErr })
	logs := &capLog{}
	c := wireClient(f.srv.URL, "gpt-6-luna", "auto", "", logs.log)
	if _, _, err := c.Chat(context.Background(), "sys", "first", 100); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if _, _, err := c.Chat(context.Background(), "sys", "second", 100); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if f.calls.Load() != 3 {
		t.Fatalf("requests = %d, want 3 (only the first call retried)", f.calls.Load())
	}
	if _, ok := f.request(t, 2)["max_completion_tokens"]; !ok {
		t.Fatalf("second call did not reuse the adaptation: %v", f.request(t, 2))
	}
	// Fleet mode builds one client per database: a second client for the
	// same endpoint and model starts adapted.
	other := wireClient(f.srv.URL, "gpt-6-luna", "", "", logs.log)
	if _, _, err := other.Chat(context.Background(), "sys", "third", 100); err != nil {
		t.Fatalf("other client: %v", err)
	}
	if f.calls.Load() != 4 {
		t.Fatalf("requests = %d, want 4: the other client re-learned", f.calls.Load())
	}
	if n := logs.count("max_completion_tokens"); n != 1 {
		t.Fatalf("adaptation logged %d times, want once per model", n)
	}
}

// The memory is keyed by endpoint and model: another model on the same
// endpoint, and the same model on another endpoint, keep max_tokens.
func TestChat_AdaptationScopedToEndpointAndModel(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.maxTokensErr = openAIMaxTokensErr })
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	if _, _, err := c.Chat(context.Background(), "sys", "u", 100); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	plain := newFakeOpenAI(t, nil)
	sameModel := wireClient(plain.srv.URL, "gpt-6-luna", "", "", noopLog)
	if _, _, err := sameModel.Chat(context.Background(), "sys", "u", 100); err != nil {
		t.Fatalf("same model, other endpoint: %v", err)
	}
	if _, ok := plain.request(t, 0)["max_tokens"]; !ok {
		t.Fatalf("other endpoint inherited the adaptation: %v", plain.request(t, 0))
	}
	otherModel := wireClient(plain.srv.URL, "gpt-4o-mini", "", "", noopLog)
	if _, _, err := otherModel.Chat(context.Background(), "sys", "v", 100); err != nil {
		t.Fatalf("other model: %v", err)
	}
	if _, ok := plain.request(t, 1)["max_tokens"]; !ok {
		t.Fatalf("other model inherited the adaptation: %v", plain.request(t, 1))
	}
}

// After adapting, a different 400 is a normal provider failure: no
// further retry, one breaker failure, the provider's message returned.
func TestChat_SecondDifferent400AfterAdaptingDoesNotRetry(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) {
		f.maxTokensErr = openAIMaxTokensErr
		f.next = func(w http.ResponseWriter, _ map[string]any) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"context too long"}}`))
		}
	})
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	_, tokens, err := c.Chat(context.Background(), "sys", "u", 100)
	if err == nil || !strings.Contains(err.Error(), "400") ||
		!strings.Contains(err.Error(), "context too long") {
		t.Fatalf("err = %v, want the second 400", err)
	}
	if tokens != 0 || c.TokensUsedToday() != 0 {
		t.Fatalf("a failed call charged tokens: %d / %d", tokens, c.TokensUsedToday())
	}
	if f.calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2 (no retry ladder)", f.calls.Load())
	}
	if failureCount(c) != 1 {
		t.Fatalf("breaker failures = %d, want exactly 1", failureCount(c))
	}
}

// A provider that keeps asking for max_completion_tokens after it was
// sent never makes the client loop.
func TestChat_RepeatedAdaptationErrorDoesNotLoop(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) {
		f.maxTokensErr = openAIMaxTokensErr
		f.next = func(w http.ResponseWriter, _ map[string]any) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(openAIMaxTokensErr))
		}
	})
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	if _, _, err := c.Chat(context.Background(), "sys", "u", 100); err == nil {
		t.Fatal("Chat succeeded against a provider that rejects every request")
	}
	if f.calls.Load() != 2 || failureCount(c) != 1 {
		t.Fatalf("requests=%d failures=%d, want 2/1", f.calls.Load(), failureCount(c))
	}
}

// Bodies that must and must not trigger the token-parameter adaptation.
func TestChat_TokenParameterErrorVariants(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		adapt  bool
	}{
		{"openai json", 400, openAIMaxTokensErr, true},
		{"plain text proxy", 400, "Unsupported parameter: 'max_tokens' is not " +
			"supported with this model. Use 'max_completion_tokens' instead.", true},
		{"upper case", 400, `{"error":{"message":"UNSUPPORTED PARAMETER: ` +
			`'MAX_TOKENS'. USE 'MAX_COMPLETION_TOKENS' INSTEAD."}}`, true},
		{"param only", 400, `{"error":{"message":"Unsupported parameter.",` +
			`"param":"max_tokens","code":"unsupported_parameter"}}`, true},
		{"empty body", 400, "", false},
		{"malformed json", 400, `{"error":`, false},
		{"cap too large", 400, `{"error":{"message":"max_tokens is too large: ` +
			`32768. This model supports at most 16384 completion tokens."}}`, false},
		{"other param", 400, `{"error":{"message":"Unsupported parameter: ` +
			`'temperature'.","param":"temperature"}}`, false},
		{"server error", 500, openAIMaxTokensErr, false},
		{"unprocessable", 422, openAIMaxTokensErr, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOpenAI(t, func(f *fakeOpenAI) {
				f.maxTokensErr, f.status = tc.body, tc.status
			})
			c := wireClient(f.srv.URL, fmt.Sprintf("gpt-6-luna-%d", i), "", "", noopLog)
			_, _, err := c.Chat(context.Background(), "sys", "u", 100)
			if tc.adapt {
				if err != nil || f.calls.Load() != 2 || failureCount(c) != 0 {
					t.Fatalf("err=%v requests=%d failures=%d, want adapted retry",
						err, f.calls.Load(), failureCount(c))
				}
				return
			}
			if err == nil || f.calls.Load() != 1 || failureCount(c) != 1 {
				t.Fatalf("err=%v requests=%d failures=%d, want one failed request",
					err, f.calls.Load(), failureCount(c))
			}
		})
	}
}

// Explicit llm.token_parameter values are honoured and never adapted.
func TestChat_ExplicitTokenParameter(t *testing.T) {
	f := newFakeOpenAI(t, nil)
	c := wireClient(f.srv.URL, "gpt-6-luna", "max_completion_tokens", "", noopLog)
	if _, _, err := c.Chat(context.Background(), "sys", "u", 100); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if keys := f.keys(t, 0); keys != "max_completion_tokens,messages,model" {
		t.Fatalf("explicit max_completion_tokens request keys = %s", keys)
	}
	strict := newFakeOpenAI(t, func(f *fakeOpenAI) { f.maxTokensErr = openAIMaxTokensErr })
	pinned := wireClient(strict.srv.URL, "gpt-6-luna", "max_tokens", "", noopLog)
	_, _, err := pinned.Chat(context.Background(), "sys", "u", 100)
	if err == nil || !strings.Contains(err.Error(), "max_completion_tokens") {
		t.Fatalf("err = %v, want the provider's rejection", err)
	}
	if strict.calls.Load() != 1 || failureCount(pinned) != 1 {
		t.Fatalf("requests=%d failures=%d, want one unadapted failure",
			strict.calls.Load(), failureCount(pinned))
	}
}

// Gemini's OpenAI-compatible endpoint and every other provider that
// accepts max_tokens see byte-identical request shapes in auto mode.
func TestChat_AutoModeLeavesCompatibleProvidersUnchanged(t *testing.T) {
	for _, model := range []string{"gemini-2.5-flash", "gpt-4o-mini", "llama-3.1-70b"} {
		f := newFakeOpenAI(t, nil)
		c := wireClient(f.srv.URL, model, "", "", noopLog)
		c.cfg.JSONMode = true
		if _, _, err := c.Chat(context.Background(), "Reply in JSON", "u", 100); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		if keys := f.keys(t, 0); keys != "max_tokens,messages,model,response_format" {
			t.Fatalf("%s request keys = %s", model, keys)
		}
		if f.calls.Load() != 1 {
			t.Fatalf("%s: %d requests, want 1", model, f.calls.Load())
		}
	}
}
