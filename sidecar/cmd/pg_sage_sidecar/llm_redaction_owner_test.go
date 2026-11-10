package main

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-09: startup applies llm.redact_numeric_literals and registers its
// live owner, so a config edit flips it without a restart, even with no
// LLM client configured.
func TestLLMRedactionOwnerAppliesAndReloads(t *testing.T) {
	oldController, oldClient := configController, llmClient
	t.Cleanup(func() {
		configController, llmClient = oldController, oldClient
		llm.SetRedactNumericLiterals(true)
	})
	initial := config.DefaultConfig()
	initial.LLM.RedactNumericLiterals = false
	configController = config.NewConfigController(initial, nil)
	llmClient = nil

	registerLLMConfigOwner()
	if llm.RedactNumericLiteralsEnabled() {
		t.Fatal("startup did not apply redact_numeric_literals: false")
	}
	lc, ok := configController.EffectiveLifecycle("llm.redact_numeric_literals")
	if !ok || lc != config.LifecycleReconfigure {
		t.Fatalf("effective lifecycle = %q (%v), want reconfigure (owner registered)", lc, ok)
	}
	next := config.Clone(initial)
	next.LLM.RedactNumericLiterals = true
	res, err := configController.Apply(context.Background(),
		configController.Desired().Generation, next)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(res.PendingRestart) != 0 {
		t.Fatalf("pending restart = %v, want a live change", res.PendingRestart)
	}
	if !llm.RedactNumericLiteralsEnabled() {
		t.Fatal("live change to true did not turn numeric redaction on")
	}
}

func TestLLMRedactionOwnerWithoutControllerKeepsDefault(t *testing.T) {
	// Nil: no controller (early startup) leaves the fail-closed default on.
	oldController := configController
	t.Cleanup(func() { configController = oldController })
	configController = nil
	llm.SetRedactNumericLiterals(true)
	registerLLMRedactionOwner()
	if !llm.RedactNumericLiteralsEnabled() {
		t.Fatal("no controller turned numeric redaction off")
	}
}
