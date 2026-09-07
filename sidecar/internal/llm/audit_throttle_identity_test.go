package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func auditThrottleClient(endpoint string) *Client {
	return New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         endpoint,
		APIKey:           "audit-key",
		Model:            "audit-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100_000,
		CooldownSeconds:  300,
	}, noopLog)
}

func TestAuditCooldownIsScopedToLogicalWorkItem(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			_, _ = w.Write(testChatJSON("[]", 1))
		},
	))
	defer server.Close()
	client := auditThrottleClient(server.URL)

	for _, prompt := range []string{
		"optimize public.orders",
		"optimize public.customers",
		"prepare incident briefing",
		"explain slow query 42",
	} {
		if _, _, err := client.Chat(t.Context(), "system", prompt, 10); err != nil {
			t.Fatalf("distinct work item %q was throttled: %v", prompt, err)
		}
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("provider requests = %d, want 4", got)
	}
}

func TestAuditCooldownStillRejectsImmediateDuplicate(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			_, _ = w.Write(testChatJSON("[]", 1))
		},
	))
	defer server.Close()
	client := auditThrottleClient(server.URL)

	if _, _, err := client.Chat(t.Context(), "system", "same work", 10); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, _, err := client.Chat(t.Context(), "system", "same work", 10)
	if !errors.Is(err, ErrRequestCooldown) {
		t.Fatalf("duplicate error = %v, want ErrRequestCooldown", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
}

func TestAuditConcurrentDistinctWorkItemsAreNotMutuallyThrottled(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			_, _ = w.Write(testChatJSON("[]", 1))
		},
	))
	defer server.Close()
	client := auditThrottleClient(server.URL)

	const count = 10
	var wg sync.WaitGroup
	errorsSeen := make(chan error, count)
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := client.Chat(
				context.Background(), "system", "table-"+string(rune('A'+i)), 10,
			)
			errorsSeen <- err
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("distinct concurrent call failed: %v", err)
		}
	}
	if got := requests.Load(); got != count {
		t.Fatalf("provider requests = %d, want %d", got, count)
	}
}

func TestAuditModelDiscoveryRejectsNilHTTPClient(t *testing.T) {
	_, err := ListModelsWithClient(
		t.Context(), "https://example.com/v1", "key", nil,
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP client") {
		t.Fatalf("nil HTTP client error = %v, want actionable rejection", err)
	}
}

// Invalid/nil input is not applicable to throttle identity: Chat normalizes a
// nil context and accepts empty prompts as valid provider input. Provider error
// propagation and token-budget boundaries are covered by existing client tests.
