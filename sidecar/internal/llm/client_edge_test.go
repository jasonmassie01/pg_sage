package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// TestClientRetryBackoff verifies that the client retries on 429
// (rate limit) status codes with exponential backoff. The server
// returns 429 twice then succeeds on the third attempt.
func TestClientRetryBackoff(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Write(testChatJSON("recovered", 10))
	}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled: true, Endpoint: srv.URL + "/",
		APIKey: "k", Model: "m",
		TimeoutSeconds: 30, TokenBudgetDaily: 100000, CooldownSeconds: 10,
	}
	client := New(cfg, noopLog)
	client.httpClient.Timeout = 30 * time.Second

	content, _, err := client.Chat(context.Background(), "s", "u", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "recovered" {
		t.Errorf("content = %q, want %q", content, "recovered")
	}

	// 429 is retryable, so doWithRetry should attempt 3 times (2 failures + 1 success).
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3 (429 should trigger retry)", attempts.Load())
	}
}

// TestClientNoRetryOn500 verifies that 500 Internal Server Error is
// NOT retried (only 429 and 503 are retryable). The error should
// be returned immediately without wasting tokens on retries.
func TestClientNoRetryOn500(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled: true, Endpoint: srv.URL + "/",
		APIKey: "k", Model: "m",
		TimeoutSeconds: 5, TokenBudgetDaily: 100000, CooldownSeconds: 10,
	}
	client := New(cfg, noopLog)

	_, _, err := client.Chat(context.Background(), "s", "u", 100)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error containing '500', got: %v", err)
	}

	// 500 is not retryable, so only 1 attempt should be made.
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d, want 1 (500 should not trigger retry)", attempts.Load())
	}
}

// TestClientContextCancellation verifies that a cancelled context
// stops the HTTP request promptly.
func TestClientContextCancellation(t *testing.T) {
	// Shared channel: test body closes it after Chat returns so the
	// handler can exit. r.Context().Done() is unreliable on Windows.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled: true, Endpoint: srv.URL + "/",
		APIKey: "k", Model: "m",
		TimeoutSeconds: 30, TokenBudgetDaily: 100000, CooldownSeconds: 10,
	}
	client := New(cfg, noopLog)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, _, err := client.Chat(ctx, "s", "u", 100)
	elapsed := time.Since(start)

	// Let the handler return so the server can close cleanly.
	close(release)

	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v, expected cancellation within 5s", elapsed)
	}
}

// TestClientEmptyAPIKey verifies that an empty API key produces a
// clear "not enabled" error without making any HTTP request.
func TestClientEmptyAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when API key is empty")
	}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled: true, Endpoint: srv.URL + "/",
		APIKey: "", Model: "m",
		TimeoutSeconds: 5, TokenBudgetDaily: 100000, CooldownSeconds: 10,
	}
	client := New(cfg, noopLog)

	if client.IsEnabled() {
		t.Error("client should not be enabled with empty API key")
	}

	_, _, err := client.Chat(context.Background(), "s", "u", 100)
	if err == nil {
		t.Fatal("expected error for empty API key")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Errorf("expected 'not enabled' error, got: %v", err)
	}
}

// TestClientConcurrentRequests verifies that multiple goroutines
// calling Chat() simultaneously do not cause data races. Run with
// -race to detect issues.
func TestClientConcurrentRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(testChatJSON("ok", 5))
	}))
	defer srv.Close()

	cfg := &config.LLMConfig{
		Enabled: true, Endpoint: srv.URL + "/",
		APIKey: "k", Model: "m",
		TimeoutSeconds: 5, TokenBudgetDaily: 100000, CooldownSeconds: 10,
	}
	client := New(cfg, noopLog)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			content, tokens, err := client.Chat(
				context.Background(), "system", "user", 100,
			)
			if err != nil {
				errs <- err
				return
			}
			if content != "ok" {
				errs <- fmt.Errorf("content = %q, want %q", content, "ok")
				return
			}
			if tokens != 5 {
				errs <- fmt.Errorf("tokens = %d, want 5", tokens)
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent error: %v", err)
	}

	// All goroutines should have added to token budget.
	total := client.TokensUsedToday()
	if total != int64(goroutines*5) {
		t.Errorf("tokens used = %d, want %d", total, goroutines*5)
	}
}
