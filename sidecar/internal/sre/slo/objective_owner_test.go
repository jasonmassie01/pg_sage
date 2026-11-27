package slo

import (
	"testing"
	"time"
)

// playbooks-08: the owner database is part of an objective's definition.
// No concurrent access tests: Hash is a pure function of the value.

func ownerObjective() Objective {
	return Objective{Name: "payments", Database: "orders", Kind: KindApp,
		Source: SourcePush, Target: 0.995, Window: 7 * 24 * time.Hour, MinEligible: 10}
}

// Pinned: an objective with no owner hashes exactly as before
// playbooks-08, so upgrading keeps every existing SLO's history.
func TestObjective_NoOwnerKeepsLegacyHash(t *testing.T) {
	const legacy = "0ed14353ac2a0e09abd83cf65ebf1517"
	if got := ownerObjective().Hash(); got != legacy {
		t.Fatalf("hash without an owner = %s, want the pre-owner %s", got, legacy)
	}
}

func TestObjective_OwnerDatabaseChangesHash(t *testing.T) {
	bare := ownerObjective()
	owned := bare
	owned.OwnerDatabase = "billing"
	other := bare
	other.OwnerDatabase = "orders"
	if owned.Hash() == bare.Hash() {
		t.Fatal("setting owner_database left the definition hash unchanged")
	}
	if owned.Hash() == other.Hash() {
		t.Fatal("two owner databases share a definition hash")
	}
	if owned.Hash() != owned.Hash() || len(owned.Hash()) != 32 {
		t.Fatalf("hash is not a stable 32-hex value: %q", owned.Hash())
	}
	if err := owned.Validate(); err != nil {
		t.Fatalf("an owned objective is invalid: %v", err)
	}
}

func TestObjective_OwnerDatabaseMustBeText(t *testing.T) {
	o := ownerObjective()
	o.OwnerDatabase = "bill\ning"
	if err := o.Validate(); err == nil {
		t.Fatal("an owner database with a control character was accepted")
	}
}
