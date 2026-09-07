package wal

import (
	"testing"
	"time"
)

func TestDefaultPolicyMatchesF4SafetyContract(t *testing.T) {
	policy := DefaultPolicy()
	if policy.AbandonAfter != 24*time.Hour {
		t.Fatalf("abandon after = %s, want 24h", policy.AbandonAfter)
	}
	if policy.RetainedWALDiskPctCeiling != 10 {
		t.Fatalf(
			"retained WAL disk ceiling = %.1f, want 10",
			policy.RetainedWALDiskPctCeiling,
		)
	}
	if policy.AllowDrop {
		t.Fatal("slot dropping must be opt-in by default")
	}
}

func TestDecisionVocabularyIsStable(t *testing.T) {
	wants := map[Action]string{
		ActionKeep:  "keep",
		ActionBound: "bound",
		ActionPark:  "park",
		ActionDrop:  "drop",
	}
	for action, want := range wants {
		if string(action) != want {
			t.Errorf("action %v serializes as %q, want %q", action, action, want)
		}
	}
}

// No concurrent-access test: Classify is a stateless function whose inputs are values.
// No integration test: this package classifies evidence and performs no database I/O.
