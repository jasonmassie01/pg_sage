package store

import (
	"testing"
	"time"
)

// fleetpol-01d: the tier rules that need no database.

func TestDatabaseTier_ValidTierAndCaps(t *testing.T) {
	cases := []struct {
		tier  string
		valid bool
		cap   int
	}{
		{TierFull, true, 50},
		{TierObserveOnly, true, 2000},
		{"", false, 0},
		{"light", false, 0},
		{"FULL", false, 0},
		{"observe_only ", false, 0},
	}
	for _, c := range cases {
		if got := ValidTier(c.tier); got != c.valid {
			t.Errorf("ValidTier(%q) = %v, want %v", c.tier, got, c.valid)
		}
		if got := TierCap(c.tier); got != c.cap {
			t.Errorf("TierCap(%q) = %d, want %d", c.tier, got, c.cap)
		}
	}
}

func TestDatabaseRecord_RunsRuntimeOnlyForEnabledFullMembers(t *testing.T) {
	cases := []struct {
		rec  DatabaseRecord
		want bool
	}{
		{DatabaseRecord{Enabled: true, Tier: TierFull}, true},
		{DatabaseRecord{Enabled: false, Tier: TierFull}, false},
		{DatabaseRecord{Enabled: true, Tier: TierObserveOnly}, false},
		// An unknown or unread tier never gets a runtime: fail closed.
		{DatabaseRecord{Enabled: true, Tier: ""}, false},
		{DatabaseRecord{Enabled: true, Tier: "light"}, false},
	}
	for _, c := range cases {
		if got := c.rec.RunsRuntime(); got != c.want {
			t.Errorf("RunsRuntime(enabled=%v tier=%q) = %v, want %v",
				c.rec.Enabled, c.rec.Tier, got, c.want)
		}
	}
}

func TestDiscoveredMember_RefusesAnUnknownTier(t *testing.T) {
	m := DiscoveredMember{Source: "s", Owner: "o", Tier: "light",
		ExpiresAt: time.Now().Add(time.Hour)}
	if err := m.validate(); err == nil {
		t.Fatal("a discovered member with tier light validated")
	}
	m.Tier = ""
	if err := m.validate(); err != nil {
		t.Fatalf("an empty tier (full) refused: %v", err)
	}
	m.Tier = TierObserveOnly
	if err := m.validate(); err != nil {
		t.Fatalf("observe_only refused: %v", err)
	}
}
