package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func wave4ClientConfig(endpoint string, budget, cooldown int) *config.LLMConfig {
	return &config.LLMConfig{
		Enabled:          true,
		Endpoint:         endpoint,
		APIKey:           "wave4-key",
		Model:            "wave4-model",
		TimeoutSeconds:   2,
		TokenBudgetDaily: budget,
		CooldownSeconds:  cooldown,
	}
}

func wave4Response(tokens int) []byte {
	response := ChatResponse{}
	response.Choices = append(response.Choices, struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}{})
	response.Choices[0].Message.Content = "ok"
	response.Usage.TotalTokens = tokens
	body, _ := json.Marshal(response)
	return body
}

func TestWave4BudgetReservationIsAtomicAndReconcilesActualUsage(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	received := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		received <- struct{}{}
		<-release
		_, _ = w.Write(wave4Response(80))
	}))
	defer server.Close()

	client := New(wave4ClientConfig(server.URL, 100, 0), func(string, string, ...any) {})
	type result struct {
		tokens int
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			_, tokens, err := client.Chat(t.Context(), "system", "user", 80)
			results <- result{tokens: tokens, err: err}
		}()
	}

	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("first admitted request did not reach provider")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)

	var successes, budgetErrors int
	for range 2 {
		result := <-results
		if result.err == nil {
			successes++
			if result.tokens != 80 {
				t.Fatalf("successful call tokens = %d, want 80", result.tokens)
			}
		} else if strings.Contains(result.err.Error(), "budget") {
			budgetErrors++
		}
	}
	if successes != 1 || budgetErrors != 1 {
		t.Fatalf("successes=%d budget_errors=%d, want 1 each", successes, budgetErrors)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
	if got := client.TokensUsedToday(); got != 80 {
		t.Fatalf("reconciled tokens = %d, want 80", got)
	}
}

func TestWave4ThinkingOverheadParticipatesInAdmission(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(wave4Response(1))
	}))
	defer server.Close()

	cfg := wave4ClientConfig(server.URL, 100, 0)
	cfg.Model = "gemini-3-pro-preview"
	client := New(cfg, func(string, string, ...any) {})
	_, _, err := client.Chat(t.Context(), "system", "user", 10)
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expected admission budget error, got %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("provider requests = %d, want 0", got)
	}
}

func TestWave4CooldownThrottlesRepeatedKey(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(wave4Response(1))
	}))
	defer server.Close()

	client := New(wave4ClientConfig(server.URL, 1_000, 1), func(string, string, ...any) {})
	if _, _, err := client.Chat(t.Context(), "system", "user", 10); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, _, err := client.Chat(t.Context(), "system", "user", 10); err == nil ||
		!strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("second call should be throttled, got %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("provider requests during cooldown = %d, want 1", got)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, _, err := client.Chat(t.Context(), "system", "user", 10); err != nil {
		t.Fatalf("call after cooldown: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("provider requests after cooldown = %d, want 2", got)
	}
}

func TestWave4MalformedSuccessCountsTowardCircuitBreaker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":`))
	}))
	defer server.Close()

	client := New(wave4ClientConfig(server.URL, 1_000, 0), func(string, string, ...any) {})
	for i := 0; i < 3; i++ {
		if _, _, err := client.Chat(t.Context(), "system", "user", 10); err == nil {
			t.Fatalf("call %d unexpectedly succeeded", i+1)
		}
	}
	if !client.IsCircuitOpen() {
		t.Fatal("malformed HTTP 200 responses did not open circuit")
	}
}

func TestWave4ModelCacheIsKeyedByEndpointAndCredential(t *testing.T) {
	InvalidateModelCache()
	t.Cleanup(InvalidateModelCache)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		_, _ = w.Write([]byte(`{"data":[{"id":"` + id + `"}]}`))
	}))
	defer server.Close()

	first, err := ListModels(t.Context(), server.URL+"/tenant-a", "credential-a")
	if err != nil {
		t.Fatalf("first list: %v", err)
	}
	second, err := ListModels(t.Context(), server.URL+"/tenant-b", "credential-b")
	if err != nil {
		t.Fatalf("second list: %v", err)
	}
	third, err := ListModels(t.Context(), server.URL+"/tenant-a", "credential-c")
	if err != nil {
		t.Fatalf("third list: %v", err)
	}
	if first[0].ID != "credential-a" || second[0].ID != "credential-b" ||
		third[0].ID != "credential-c" {
		t.Fatalf("cache crossed identities: first=%v second=%v third=%v", first, second, third)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("model-list calls = %d, want 3", got)
	}
}

func TestWave4ProviderErrorsRedactURLSecrets(t *testing.T) {
	const secret = "gemini-secret-value"
	_, err := doModelRequest(t.Context(), "://models?key="+secret, "")
	if err == nil {
		t.Fatal("invalid provider URL unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("provider error leaked URL secret: %v", err)
	}
}

func TestWave4LateResponseRejectedAfterDisable(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(received)
		<-release
		_, _ = w.Write(wave4Response(25))
	}))
	defer server.Close()

	cfg := wave4ClientConfig(server.URL, 1_000, 0)
	client := New(cfg, func(string, string, ...any) {})
	var wg sync.WaitGroup
	wg.Add(1)
	var tokens int
	var callErr error
	go func() {
		defer wg.Done()
		_, tokens, callErr = client.Chat(context.Background(), "system", "user", 50)
	}()
	<-received
	disabled := *cfg
	disabled.Enabled = false
	client.Reconfigure(&disabled)
	close(release)
	wg.Wait()

	if callErr == nil || (!errors.Is(callErr, context.Canceled) &&
		!strings.Contains(callErr.Error(), "disabled")) {
		t.Fatalf("late response after disable was accepted: err=%v", callErr)
	}
	if tokens != 0 || client.TokensUsedToday() != 0 {
		t.Fatalf("disabled late response consumed tokens: call=%d total=%d",
			tokens, client.TokensUsedToday())
	}
}

func TestWave4ReconfigureAppliesNewRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		_, _ = w.Write(wave4Response(1))
	}))
	defer server.Close()

	cfg := wave4ClientConfig(server.URL, 1_000, 0)
	cfg.TimeoutSeconds = 5
	client := New(cfg, func(string, string, ...any) {})
	next := *cfg
	next.TimeoutSeconds = 1
	client.Reconfigure(&next)

	started := time.Now()
	_, _, err := client.Chat(t.Context(), "system", "user", 10)
	if err == nil {
		t.Fatal("request ignored reconfigured timeout")
	}
	if elapsed := time.Since(started); elapsed > 1300*time.Millisecond {
		t.Fatalf("reconfigured timeout took %s, want about 1s", elapsed)
	}
}
