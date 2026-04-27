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

func TestTokenBudgetDaily(t *testing.T) {
	cfg := &config.LLMConfig{TokenBudgetDaily: 50000}
	client := New(cfg, noopLog)

	if got := client.TokenBudgetDaily(); got != 50000 {
		t.Errorf("TokenBudgetDaily() = %d, want 50000", got)
	}
}

func TestTokenBudgetDaily_Unlimited(t *testing.T) {
	cfg := &config.LLMConfig{TokenBudgetDaily: 0}
	client := New(cfg, noopLog)

	if got := client.TokenBudgetDaily(); got != 0 {
		t.Errorf("TokenBudgetDaily() = %d, want 0", got)
	}
}

func TestModel(t *testing.T) {
	cfg := &config.LLMConfig{Model: "gemini-2.5-flash"}
	client := New(cfg, noopLog)

	if got := client.Model(); got != "gemini-2.5-flash" {
		t.Errorf("Model() = %q, want %q", got, "gemini-2.5-flash")
	}
}

func TestIsBudgetExhausted_BelowLimit(t *testing.T) {
	cfg := &config.LLMConfig{TokenBudgetDaily: 10000}
	client := New(cfg, noopLog)
	client.budgetResetDay = time.Now().YearDay()
	client.tokensUsedToday.Store(5000)

	if client.IsBudgetExhausted() {
		t.Error("IsBudgetExhausted() = true, want false")
	}
}

func TestIsBudgetExhausted_AtLimit(t *testing.T) {
	cfg := &config.LLMConfig{TokenBudgetDaily: 10000}
	client := New(cfg, noopLog)
	client.budgetResetDay = time.Now().YearDay()
	client.tokensUsedToday.Store(10000)

	if !client.IsBudgetExhausted() {
		t.Error("IsBudgetExhausted() = false, want true")
	}
}

func TestIsBudgetExhausted_AboveLimit(t *testing.T) {
	cfg := &config.LLMConfig{TokenBudgetDaily: 10000}
	client := New(cfg, noopLog)
	client.budgetResetDay = time.Now().YearDay()
	client.tokensUsedToday.Store(15000)

	if !client.IsBudgetExhausted() {
		t.Error("IsBudgetExhausted() = false, want true")
	}
}

func TestIsBudgetExhausted_NoBudget(t *testing.T) {
	cfg := &config.LLMConfig{TokenBudgetDaily: 0}
	client := New(cfg, noopLog)
	client.tokensUsedToday.Store(999999)

	if client.IsBudgetExhausted() {
		t.Error("IsBudgetExhausted() should be false with no budget")
	}
}

func TestIsBudgetExhausted_DayReset(t *testing.T) {
	cfg := &config.LLMConfig{TokenBudgetDaily: 100}
	client := New(cfg, noopLog)
	// Set reset day to yesterday so tokens should be considered reset.
	client.budgetResetDay = time.Now().YearDay() - 1
	client.tokensUsedToday.Store(200)

	if client.IsBudgetExhausted() {
		t.Error("IsBudgetExhausted() should be false after day rollover")
	}
}

func TestResetBudget(t *testing.T) {
	cfg := &config.LLMConfig{TokenBudgetDaily: 1000}
	client := New(cfg, noopLog)
	client.budgetResetDay = time.Now().YearDay()
	client.tokensUsedToday.Store(5000)

	if !client.IsBudgetExhausted() {
		t.Fatal("precondition: budget should be exhausted")
	}

	client.ResetBudget()

	if client.TokensUsedToday() != 0 {
		t.Errorf("TokensUsedToday = %d after reset, want 0",
			client.TokensUsedToday())
	}
	if client.IsBudgetExhausted() {
		t.Error("budget should not be exhausted after reset")
	}
}

func TestChat_JSONModeSendsResponseFormat(t *testing.T) {
	var captured ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Errorf("decode: %v", err)
			}
			w.Write(testChatJSON("{}", 1))
		},
	))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL + "/",
		APIKey:           "k",
		Model:            "m",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 1000,
		JSONMode:         true,
	}
	client := New(cfg, noopLog)
	if _, _, err := client.Chat(
		context.Background(), "sys", "user", 50,
	); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if captured.ResponseFormat == nil {
		t.Fatal("response_format not sent")
	}
	if captured.ResponseFormat.Type != "json_object" {
		t.Errorf("response_format.type = %q, want json_object",
			captured.ResponseFormat.Type)
	}
}

func TestChat_JSONModeOffOmitsResponseFormat(t *testing.T) {
	// Default: JSONMode false. Providers that reject unknown fields
	// must not see response_format at all.
	var rawBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&rawBody)
			w.Write(testChatJSON("{}", 1))
		},
	))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL + "/",
		APIKey:           "k",
		Model:            "m",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 1000,
		// JSONMode left false.
	}
	client := New(cfg, noopLog)
	if _, _, err := client.Chat(
		context.Background(), "sys", "user", 50,
	); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, ok := rawBody["response_format"]; ok {
		t.Error("response_format leaked into request when JSONMode=false")
	}
}
