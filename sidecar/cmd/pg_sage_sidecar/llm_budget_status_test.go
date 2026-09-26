package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// fakeLLMProvider answers every chat request with a fixed token count.
func fakeLLMProvider(t *testing.T, tokens int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"ok"},`+
				`"finish_reason":"stop"}],"usage":{"total_tokens":%d}}`, tokens)
		}))
	t.Cleanup(server.Close)
	return server
}

func pointLLMAt(t *testing.T, url string) {
	t.Helper()
	applyLLMChange(t, func(l *config.LLMConfig) {
		l.Endpoint = url
		l.TokenBudgetDaily = 100000
	})
}

// G3-B14: status must report every client the sidecar can spend through,
// and reset must zero all of them, not only the shared general client.
func TestLLMBudgetStatusAndResetCoverEveryClient(t *testing.T) {
	llmTestGlobals(t)
	pointLLMAt(t, fakeLLMProvider(t, 40).URL)
	dbGeneral, dbManager := newFleetDBLLMClients("orders", true)
	ctx := context.Background()
	if _, _, err := dbGeneral.Chat(ctx, "sys", "orders prompt", 10); err != nil {
		t.Fatalf("per-database chat: %v", err)
	}
	if _, _, err := llmClient.Chat(ctx, "sys", "shared prompt", 10); err != nil {
		t.Fatalf("shared chat: %v", err)
	}

	status := llmClients.TokenStatus()
	for _, key := range []string{"general", "orders/general", "orders/optimizer"} {
		if _, ok := status[key]; !ok {
			t.Fatalf("status keys = %v, missing %q", keys(status), key)
		}
	}
	if got := status["orders/general"].TokensUsed; got != 40 {
		t.Fatalf("orders/general tokens = %d, want 40", got)
	}
	if got := status["general"].TokensUsed; got != 40 {
		t.Fatalf("general tokens = %d, want 40", got)
	}

	llmClients.ResetBudgets()

	if dbGeneral.TokensUsedToday() != 0 || llmClient.TokensUsedToday() != 0 ||
		dbManager.Optimizer.TokensUsedToday() != 0 {
		t.Fatal("reset left spend on a registered client")
	}
}

// The fleet-wide per-database budget is part of the reported and reset state.
func TestLLMBudgetStatusIncludesAndResetsFleetBudget(t *testing.T) {
	llmTestGlobals(t)
	cfg.LLM.FleetTokenBudgetDaily = 1000
	initializeFleetBudget(nil)
	pointLLMAt(t, fakeLLMProvider(t, 25).URL)
	dbGeneral, _ := newFleetDBLLMClients("billing", true)
	if _, _, err := dbGeneral.Chat(context.Background(), "sys", "p", 10); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got := fleetLLMBudget.Used("billing"); got != 25 {
		t.Fatalf("precondition: fleet budget used = %d, want 25", got)
	}

	entry, ok := llmClients.TokenStatus()["billing/fleet_budget"]
	if !ok || entry.TokensUsed != 25 || entry.TokenBudget != 1000 {
		t.Fatalf("fleet budget entry = %+v (present=%v), want used 25 of 1000",
			entry, ok)
	}
	llmClients.ResetBudgets()
	if got := fleetLLMBudget.Used("billing"); got != 0 {
		t.Fatalf("fleet budget used after reset = %d, want 0", got)
	}
}

// A retired runtime generation disappears from the status.
func TestLLMBudgetStatusDropsRemovedClients(t *testing.T) {
	llmTestGlobals(t)
	general, manager := newFleetDBLLMClients("gone", true)
	llmClients.remove(general, manager.Optimizer)
	for key := range llmClients.TokenStatus() {
		if key == "gone/general" || key == "gone/optimizer" {
			t.Fatalf("removed client %q still reported", key)
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
