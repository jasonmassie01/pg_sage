package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

func (r *llmClientRegistry) tracked() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// A runtime's clients are untracked from the registry that tracked them,
// even if the process registry was replaced meanwhile (tests replace it).
func TestReleaseLLMClientsUntracksFromOwningRegistry(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	owner := &llmClientRegistry{}
	llmClients = owner
	general := owner.newClient(llmRoleGeneral, "orders", true)
	ctx, cancel := context.WithCancel(context.Background())
	releaseLLMClientsOnDone(ctx, llm.NewManager(general, nil, false))
	replacement := &llmClientRegistry{}
	replacement.newClient(llmRoleGeneral, "other", true)
	llmClients = replacement

	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for owner.tracked() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if owner.tracked() != 0 {
		t.Fatal("a finished runtime's client is still tracked by its registry")
	}
	if replacement.tracked() != 1 {
		t.Fatalf("another registry lost clients: %d tracked, want 1", replacement.tracked())
	}
}

func TestReleaseLLMClientsIgnoresMissingManager(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	releaseLLMClientsOnDone(ctx, nil)
	if llmClients.tracked() != 0 {
		t.Fatal("a nil manager changed the registry")
	}
}
