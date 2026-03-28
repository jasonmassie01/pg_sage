package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func noopLog(_, _ string, _ ...any) {}

// testChatJSON builds a mock OpenAI chat response body.
func testChatJSON(content string, tokens int) []byte {
	resp := map[string]any{
		"choices": []map[string]any{
			{
				"message":       map[string]string{"content": content},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]int{"total_tokens": tokens},
	}
	b, _ := json.Marshal(resp)
	return b
}

func TestChat_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(testChatJSON("hello world", 42))
	}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL + "/",
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}

	client := New(cfg, noopLog)
	content, tokens, err := client.Chat(context.Background(), "system", "user", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "hello world" {
		t.Errorf("content = %q, want %q", content, "hello world")
	}
	if tokens != 42 {
		t.Errorf("tokens = %d, want 42", tokens)
	}
}

func TestChat_BudgetExhausted(t *testing.T) {
	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/",
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 10,
		CooldownSeconds:  10,
	}

	client := New(cfg, noopLog)
	// Set budget reset day to today so the daily reset doesn't clear our tokens.
	client.budgetResetDay = time.Now().YearDay()
	client.tokensUsedToday.Store(10)

	_, _, err := client.Chat(context.Background(), "system", "user", 100)
	if err == nil {
		t.Fatal("expected budget exhaustion error")
	}
	if got := err.Error(); got != "daily token budget exhausted (10/10)" {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestChat_CircuitBreaker(t *testing.T) {
	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/",
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  3600,
	}

	client := New(cfg, noopLog)
	// Trigger circuit breaker by recording 3 failures.
	client.recordFailure()
	client.recordFailure()
	client.recordFailure()

	if !client.IsCircuitOpen() {
		t.Fatal("expected circuit to be open")
	}

	_, _, err := client.Chat(context.Background(), "system", "user", 100)
	if err == nil {
		t.Fatal("expected circuit breaker error")
	}
}

func TestChat_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ChatResponse{}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL + "/",
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}

	client := New(cfg, noopLog)
	_, _, err := client.Chat(context.Background(), "system", "user", 100)
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
}

func TestChat_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL + "/",
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}

	client := New(cfg, noopLog)
	_, _, err := client.Chat(context.Background(), "system", "user", 100)
	if err == nil {
		t.Fatal("expected error for server error")
	}
}
