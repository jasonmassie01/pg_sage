package llm

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// State transitions: the llm_redaction owner applies a live change of
// llm.redact_numeric_literals on Commit and restores it on Rollback.
func TestRedactionOwnerCommitAndRollback(t *testing.T) {
	defer SetRedactNumericLiterals(true)
	owner := RedactionOwner{}
	if owner.Name() != config.LLMRedactionOwner {
		t.Fatalf("Name = %q, want %q", owner.Name(), config.LLMRedactionOwner)
	}
	on, off := config.DefaultConfig(), config.DefaultConfig()
	off.LLM.RedactNumericLiterals = false
	prep, err := owner.Prepare(context.Background(),
		config.ConfigSnapshot{Config: on}, config.ConfigSnapshot{Config: off})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !RedactNumericLiteralsEnabled() {
		t.Fatal("Prepare changed the switch before Commit")
	}
	if err := prep.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if RedactNumericLiteralsEnabled() {
		t.Fatal("Commit of false left numeric redaction on")
	}
	if err := prep.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !RedactNumericLiteralsEnabled() {
		t.Fatal("Rollback did not restore numeric redaction")
	}
	if err := prep.Drain(context.Background()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

func TestRedactionOwnerNilSnapshotsKeepRedactionOn(t *testing.T) {
	// Nil: a snapshot without a config must fail closed (redact).
	defer SetRedactNumericLiterals(true)
	SetRedactNumericLiterals(false)
	prep, err := RedactionOwner{}.Prepare(context.Background(),
		config.ConfigSnapshot{}, config.ConfigSnapshot{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := prep.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !RedactNumericLiteralsEnabled() {
		t.Fatal("nil desired config turned numeric redaction off")
	}
}

func TestApplyRedactionConfig(t *testing.T) {
	defer SetRedactNumericLiterals(true)
	ApplyRedactionConfig(&config.LLMConfig{RedactNumericLiterals: false})
	if RedactNumericLiteralsEnabled() {
		t.Fatal("ApplyRedactionConfig(false) left redaction on")
	}
	ApplyRedactionConfig(nil)
	if !RedactNumericLiteralsEnabled() {
		t.Fatal("ApplyRedactionConfig(nil) did not fail closed to on")
	}
}
