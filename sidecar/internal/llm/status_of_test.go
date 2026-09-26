package llm

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// StatusOf lets the sidecar's registry report clients outside a Manager
// (G3-B14); it must match what a Manager reports for the same client.
func TestStatusOfMatchesManagerStatus(t *testing.T) {
	client := New(&config.LLMConfig{
		Enabled: true, Model: "m", TokenBudgetDaily: 77,
	}, nil)
	got := StatusOf(client)
	want := NewManager(client, nil, false).TokenStatus()["general"]
	if got != want {
		t.Fatalf("StatusOf = %+v, want %+v", got, want)
	}
	if got.TokenBudget != 77 || got.Model != "m" || got.Exhausted {
		t.Fatalf("status = %+v, want budget 77 model m not exhausted", got)
	}
}
