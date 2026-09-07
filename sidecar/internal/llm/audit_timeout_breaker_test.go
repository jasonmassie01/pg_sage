package llm

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

type auditRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn auditRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func auditTimeoutClient() *Client {
	client := New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         "https://provider.invalid/v1",
		APIKey:           "audit-key",
		Model:            "audit-model",
		TimeoutSeconds:   1,
		TokenBudgetDaily: 100_000,
		CooldownSeconds:  0,
	}, noopLog)
	client.httpClient.Transport = auditRoundTripFunc(
		func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		},
	)
	return client
}

func TestAuditProviderTimeoutsOpenCircuit(t *testing.T) {
	client := auditTimeoutClient()
	for i := 0; i < 3; i++ {
		_, _, err := client.Chat(t.Context(), "system", "work", 10)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout %d error = %v, want deadline exceeded", i+1, err)
		}
	}
	if !client.IsCircuitOpen() {
		t.Fatal("three provider timeouts did not open circuit")
	}
}

func TestAuditCallerCancellationDoesNotPoisonCircuit(t *testing.T) {
	client := auditTimeoutClient()
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := client.Chat(ctx, "system", "work", 10)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation %d error = %v, want canceled", i+1, err)
		}
	}
	if client.IsCircuitOpen() {
		t.Fatal("caller cancellations opened provider circuit")
	}
}

func TestAuditProviderTimeoutBoundary(t *testing.T) {
	client := auditTimeoutClient()
	for i := 0; i < 2; i++ {
		if _, _, err := client.Chat(t.Context(), "system", "work", 10); err == nil {
			t.Fatalf("timeout %d unexpectedly succeeded", i+1)
		}
	}
	if client.IsCircuitOpen() {
		t.Fatal("circuit opened before the third provider timeout")
	}
	if _, _, err := client.Chat(t.Context(), "system", "work", 10); err == nil {
		t.Fatal("third timeout unexpectedly succeeded")
	}
	if !client.IsCircuitOpen() {
		t.Fatal("circuit remained closed at timeout threshold")
	}
}

func TestAuditConcurrentProviderTimeoutsOpenCircuit(t *testing.T) {
	client := auditTimeoutClient()
	const count = 3
	var wg sync.WaitGroup
	errorsSeen := make(chan error, count)
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := client.Chat(context.Background(), "system", "work", 10)
			errorsSeen <- err
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("concurrent timeout error = %v", err)
		}
	}
	if !client.IsCircuitOpen() {
		t.Fatal("concurrent provider timeouts did not open circuit")
	}
}

// Nil/empty/zero config behavior is covered by client controls tests. There is
// no database integration case: the breaker wraps an outbound HTTP provider.
