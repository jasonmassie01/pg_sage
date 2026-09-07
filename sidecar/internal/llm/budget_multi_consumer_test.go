package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
)

type scopedFleetBudget struct {
	budget   *fleet.FleetBudget
	database string
}

func (b scopedFleetBudget) CanSpend(tokens int) bool {
	return b.budget.CanSpend(b.database, tokens)
}

func (b scopedFleetBudget) Spend(tokens int) {
	b.budget.Spend(b.database, tokens)
}

func TestFleetBudgetReservationsAreAtomicAcrossLLMConsumers(t *testing.T) {
	const (
		database  = "noisy-db"
		consumers = 4
		tokens    = 4
		budgetCap = 12
	)

	var requests atomic.Int32
	gate := make(chan struct{})
	var releaseOnce sync.Once
	timer := time.AfterFunc(250*time.Millisecond, func() {
		releaseOnce.Do(func() { close(gate) })
	})
	t.Cleanup(func() { timer.Stop() })

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			if requests.Add(1) == consumers {
				releaseOnce.Do(func() { close(gate) })
			}
			<-gate
			response := map[string]any{
				"choices": []map[string]any{{
					"message": map[string]string{"content": "ok"},
				}},
				"usage": map[string]int{"total_tokens": tokens},
			}
			if err := json.NewEncoder(w).Encode(response); err != nil {
				t.Errorf("encode mock LLM response: %v", err)
			}
		},
	))
	t.Cleanup(server.Close)

	shared := fleet.NewBudget(budgetCap, []string{database})
	clients := make([]*llm.Client, consumers)
	for index := range clients {
		clients[index] = llm.New(&config.LLMConfig{
			Enabled:          true,
			Endpoint:         server.URL,
			APIKey:           "test-key",
			Model:            "test-model",
			TimeoutSeconds:   2,
			TokenBudgetDaily: 1000,
			CooldownSeconds:  0,
		}, func(string, string, ...any) {})
		clients[index].SetBudget(scopedFleetBudget{
			budget: shared, database: database,
		})
	}

	start := make(chan struct{})
	errors := make(chan error, consumers)
	var workers sync.WaitGroup
	for _, client := range clients {
		workers.Add(1)
		go func(client *llm.Client) {
			defer workers.Done()
			<-start
			_, _, err := client.Chat(
				context.Background(), "system", "user", tokens,
			)
			errors <- err
		}(client)
	}
	close(start)
	workers.Wait()
	close(errors)

	successes := 0
	rejections := 0
	for err := range errors {
		switch {
		case err == nil:
			successes++
		case strings.Contains(err.Error(), "per-database token budget exhausted"):
			rejections++
		default:
			t.Errorf("unexpected LLM error: %v", err)
		}
	}
	if successes != 3 || rejections != 1 {
		t.Fatalf("successes/rejections = %d/%d, want 3/1", successes, rejections)
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("provider requests = %d, want 3", got)
	}
	if got := shared.Used(database); got != budgetCap {
		t.Errorf("recorded spend = %d, want %d", got, budgetCap)
	}
}

func TestFleetBudgetReservationReleasedAfterProviderFailure(t *testing.T) {
	const database = "retry-db"
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
		},
	))
	t.Cleanup(server.Close)

	budget := fleet.NewBudget(4, []string{database})
	client := newScopedBudgetClient(server.URL, budget, database)
	_, _, err := client.Chat(context.Background(), "system", "failure", 4)
	if err == nil {
		t.Fatal("provider failure returned nil error")
	}
	if got := budget.Used(database); got != 0 {
		t.Fatalf("failed request retained %d reserved tokens, want 0", got)
	}
}

func TestFleetBudgetReconcilesReservationToActualUsage(t *testing.T) {
	const database = "reconcile-db"
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			response := map[string]any{
				"choices": []map[string]any{{
					"message": map[string]string{"content": "ok"},
				}},
				"usage": map[string]int{"total_tokens": 2},
			}
			if err := json.NewEncoder(w).Encode(response); err != nil {
				t.Errorf("encode mock LLM response: %v", err)
			}
		},
	))
	t.Cleanup(server.Close)

	budget := fleet.NewBudget(4, []string{database})
	client := newScopedBudgetClient(server.URL, budget, database)
	if _, _, err := client.Chat(
		context.Background(), "system", "first", 4,
	); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if got := budget.Used(database); got != 2 {
		t.Fatalf("actual spend = %d, want 2", got)
	}
	if _, _, err := client.Chat(
		context.Background(), "system", "second", 2,
	); err != nil {
		t.Fatalf("reconciled capacity was not reusable: %v", err)
	}
}

func newScopedBudgetClient(
	endpoint string,
	budget *fleet.FleetBudget,
	database string,
) *llm.Client {
	client := llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         endpoint,
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   2,
		TokenBudgetDaily: 1000,
	}, func(string, string, ...any) {})
	client.SetBudget(scopedFleetBudget{budget: budget, database: database})
	return client
}

// Nil/empty/zero budget behavior is covered by internal/llm/budget_fleet_test.go
// and internal/fleet/budget_test.go. Provider error propagation is orthogonal:
// this test isolates concurrent reservation correctness before provider I/O.
