package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// --- Budget Exhaustion Degradation ---

func TestChatForPurpose_BudgetExhausted_ReturnsError(t *testing.T) {
	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/",
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100,
		CooldownSeconds:  10,
	}
	gen := New(cfg, noopLog)
	gen.budgetResetDay.Store(int64(time.Now().YearDay()))
	gen.tokensUsedToday.Store(100) // exactly at budget

	mgr := NewManager(gen, nil, false)

	_, _, err := mgr.ChatForPurpose(
		context.Background(),
		"advisor", "system", "user", 4096,
	)
	if err == nil {
		t.Fatal("expected error when budget exhausted")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("error should mention budget exhaustion, got: %s",
			err.Error())
	}
}

func TestChatForPurpose_OptimizerExhausted_FallsBackToGeneral(
	t *testing.T,
) {
	// General client: working mock server.
	genSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Write(testChatJSON("general response", 10))
		}))
	defer genSrv.Close()

	genCfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         genSrv.URL + "/",
		APIKey:           "test-key",
		Model:            "general-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	gen := New(genCfg, noopLog)

	// Optimizer client: budget exhausted.
	optCfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/",
		APIKey:           "test-key",
		Model:            "opt-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100,
		CooldownSeconds:  10,
	}
	opt := New(optCfg, noopLog)
	opt.budgetResetDay.Store(int64(time.Now().YearDay()))
	opt.tokensUsedToday.Store(100) // exhausted

	mgr := NewManager(gen, opt, true) // fallback enabled

	content, _, err := mgr.ChatForPurpose(
		context.Background(),
		"index_optimization", "system", "user", 4096,
	)
	if err != nil {
		t.Fatalf("expected fallback to general, got error: %v",
			err)
	}
	if content != "general response" {
		t.Errorf("expected general response, got: %q", content)
	}
}

func TestChatForPurpose_OptimizerExhausted_NoFallback_ReturnsError(
	t *testing.T,
) {
	genCfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/",
		APIKey:           "test-key",
		Model:            "general-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	gen := New(genCfg, noopLog)

	optCfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/",
		APIKey:           "test-key",
		Model:            "opt-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100,
		CooldownSeconds:  10,
	}
	opt := New(optCfg, noopLog)
	opt.budgetResetDay.Store(int64(time.Now().YearDay()))
	opt.tokensUsedToday.Store(100)

	mgr := NewManager(gen, opt, false) // fallback disabled

	_, _, err := mgr.ChatForPurpose(
		context.Background(),
		"index_optimization", "system", "user", 4096,
	)
	if err == nil {
		t.Fatal("expected budget error without fallback")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("error should mention budget, got: %s",
			err.Error())
	}
}

func TestChatForPurpose_BothExhausted_FallbackStillFails(
	t *testing.T,
) {
	genCfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/",
		APIKey:           "test-key",
		Model:            "general-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 50,
		CooldownSeconds:  10,
	}
	gen := New(genCfg, noopLog)
	gen.budgetResetDay.Store(int64(time.Now().YearDay()))
	gen.tokensUsedToday.Store(50) // also exhausted

	optCfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/",
		APIKey:           "test-key",
		Model:            "opt-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100,
		CooldownSeconds:  10,
	}
	opt := New(optCfg, noopLog)
	opt.budgetResetDay.Store(int64(time.Now().YearDay()))
	opt.tokensUsedToday.Store(100)

	mgr := NewManager(gen, opt, true) // fallback enabled

	_, _, err := mgr.ChatForPurpose(
		context.Background(),
		"index_optimization", "system", "user", 4096,
	)
	if err == nil {
		t.Fatal("expected error when both clients exhausted")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("error should mention budget, got: %s",
			err.Error())
	}
}

// --- Circuit Breaker Degradation ---

func TestChatForPurpose_CircuitOpen_FallsBackToGeneral(
	t *testing.T,
) {
	genSrv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Write(testChatJSON("fallback ok", 5))
		}))
	defer genSrv.Close()

	genCfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         genSrv.URL + "/",
		APIKey:           "test-key",
		Model:            "general-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	gen := New(genCfg, noopLog)

	optCfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/",
		APIKey:           "test-key",
		Model:            "opt-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  3600,
	}
	opt := New(optCfg, noopLog)
	// Trip the circuit breaker.
	opt.recordFailure()
	opt.recordFailure()
	opt.recordFailure()

	mgr := NewManager(gen, opt, true) // fallback enabled

	content, _, err := mgr.ChatForPurpose(
		context.Background(),
		"index_optimization", "system", "user", 4096,
	)
	if err != nil {
		t.Fatalf("expected fallback to general, got error: %v",
			err)
	}
	if content != "fallback ok" {
		t.Errorf("expected fallback response, got: %q", content)
	}
}

// --- Status Reflects Exhaustion ---

func TestTokenStatus_ReflectsExhaustion(t *testing.T) {
	gen := testClient("general")
	gen.budgetResetDay.Store(int64(time.Now().YearDay()))
	gen.tokensUsedToday.Store(5000) // 5000 >= 1000 budget

	opt := testClient("optimizer")
	opt.budgetResetDay.Store(int64(time.Now().YearDay()))
	opt.tokensUsedToday.Store(500) // below budget

	mgr := NewManager(gen, opt, false)
	status := mgr.TokenStatus()

	gs := status["general"]
	if !gs.Exhausted {
		t.Error("general should be exhausted (5000 >= 1000)")
	}
	if gs.TokensUsed != 5000 {
		t.Errorf("general tokens_used = %d, want 5000",
			gs.TokensUsed)
	}

	os := status["optimizer"]
	if os.Exhausted {
		t.Error("optimizer should not be exhausted (500 < 1000)")
	}
}

func TestTokenStatus_AfterReset_NotExhausted(t *testing.T) {
	gen := testClient("general")
	gen.budgetResetDay.Store(int64(time.Now().YearDay()))
	gen.tokensUsedToday.Store(5000)

	mgr := NewManager(gen, nil, false)

	// Confirm exhaustion before reset.
	pre := mgr.TokenStatus()
	if !pre["general"].Exhausted {
		t.Fatal("precondition: general should be exhausted")
	}

	mgr.ResetBudgets()

	post := mgr.TokenStatus()
	if post["general"].Exhausted {
		t.Error("general should not be exhausted after reset")
	}
	if post["general"].TokensUsed != 0 {
		t.Errorf("tokens_used = %d after reset, want 0",
			post["general"].TokensUsed)
	}
}

// --- Disabled Client Degradation ---

func TestChat_DisabledClient_ReturnsError(t *testing.T) {
	cfg := &config.LLMConfig{
		Enabled:          false,
		Endpoint:         "http://localhost:1/",
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := New(cfg, noopLog)

	_, _, err := client.Chat(
		context.Background(), "system", "user", 100)
	if err == nil {
		t.Fatal("expected error for disabled client")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("error should mention not enabled, got: %s",
			err.Error())
	}
}

func TestChat_MissingEndpoint_ReturnsError(t *testing.T) {
	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "",
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := New(cfg, noopLog)

	_, _, err := client.Chat(
		context.Background(), "system", "user", 100)
	if err == nil {
		t.Fatal("expected error for missing endpoint")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("error should mention not enabled, got: %s",
			err.Error())
	}
}

func TestChat_MissingAPIKey_ReturnsError(t *testing.T) {
	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         "http://localhost:1/",
		APIKey:           "",
		Model:            "test-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := New(cfg, noopLog)

	_, _, err := client.Chat(
		context.Background(), "system", "user", 100)
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("error should mention not enabled, got: %s",
			err.Error())
	}
}

// --- Thinking Model Token Overhead ---

func TestChat_ThinkingModel_AddsTokenOverhead(t *testing.T) {
	var capturedMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var req ChatRequest
			json.NewDecoder(r.Body).Decode(&req)
			capturedMaxTokens = req.MaxTokens
			w.Write(testChatJSON("ok", 10))
		}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL + "/",
		APIKey:           "test-key",
		Model:            "gemini-2.5-flash",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := New(cfg, noopLog)

	_, _, err := client.Chat(
		context.Background(), "system", "user", 4096)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Thinking models get 16384 added to max_tokens.
	expected := 4096 + 16384
	if capturedMaxTokens != expected {
		t.Errorf("max_tokens = %d, want %d (4096 + 16384 overhead)",
			capturedMaxTokens, expected)
	}
}

func TestChat_NonThinkingModel_NoOverhead(t *testing.T) {
	var capturedMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var req ChatRequest
			json.NewDecoder(r.Body).Decode(&req)
			capturedMaxTokens = req.MaxTokens
			w.Write(testChatJSON("ok", 10))
		}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL + "/",
		APIKey:           "test-key",
		Model:            "gpt-4o",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := New(cfg, noopLog)

	_, _, err := client.Chat(
		context.Background(), "system", "user", 4096)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedMaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want 4096 (no overhead)",
			capturedMaxTokens)
	}
}

func TestChat_ZeroMaxTokens_DefaultsTo16384(t *testing.T) {
	var capturedMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var req ChatRequest
			json.NewDecoder(r.Body).Decode(&req)
			capturedMaxTokens = req.MaxTokens
			w.Write(testChatJSON("ok", 10))
		}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL + "/",
		APIKey:           "test-key",
		Model:            "gpt-4o",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := New(cfg, noopLog)

	_, _, err := client.Chat(
		context.Background(), "system", "user", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedMaxTokens != 16384 {
		t.Errorf("max_tokens = %d, want 16384 (default)",
			capturedMaxTokens)
	}
}

// --- Truncated Response Repair ---

func TestRepairTruncatedJSON_InChat(t *testing.T) {
	// Simulate a response with finish_reason=length (truncated).
	truncatedContent := `[{"hint":"HashJoin(t1 t2)","rationale":"good"},{"hint":"Set(work_mem`
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			resp := map[string]any{
				"choices": []map[string]any{
					{
						"message": map[string]string{
							"content": truncatedContent,
						},
						"finish_reason": "length",
					},
				},
				"usage": map[string]int{"total_tokens": 500},
			}
			json.NewEncoder(w).Encode(resp)
		}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled:          true,
		Endpoint:         srv.URL + "/",
		APIKey:           "test-key",
		Model:            "gemini-2.5-flash",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  10,
	}
	client := New(cfg, noopLog)

	content, _, err := client.Chat(
		context.Background(), "system", "user", 4096)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Repaired JSON should parse as a valid array.
	var result []map[string]string
	if jsonErr := json.Unmarshal(
		[]byte(content), &result); jsonErr != nil {
		t.Fatalf("repaired JSON should parse, got: %v "+
			"(content=%q)", jsonErr, content)
	}
	if len(result) != 1 {
		t.Errorf("expected 1 complete object after repair, "+
			"got %d", len(result))
	}
	if result[0]["hint"] != "HashJoin(t1 t2)" {
		t.Errorf("unexpected hint: %q", result[0]["hint"])
	}
}

