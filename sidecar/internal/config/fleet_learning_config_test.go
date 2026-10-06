package config

import (
	"strings"
	"testing"
	"time"
)

// fleet_learning: schema fingerprints, look-alike priors, fleet findings,
// need-based LLM budget split and leader election. On by default (it only
// adds evidence and coordination), private by default (no names stored).

func TestFleetLearningDefaults(t *testing.T) {
	f := DefaultConfig().FleetLearning
	if !f.Enabled || f.IncludeNames {
		t.Fatalf("enabled=%v include_names=%v, want on and private", f.Enabled,
			f.IncludeNames)
	}
	if f.IntervalMinutes != 60 || f.LookalikeMinSimilarity != 0.6 ||
		f.MinPriorOutcomes != 3 || f.FleetFindingMinDatabases != 3 {
		t.Fatalf("defaults = %+v", f)
	}
	if f.BudgetSplit != "need" || f.BudgetFloorPct != 50 || f.BudgetCeilingPct != 300 {
		t.Fatalf("budget defaults = %+v", f)
	}
	if f.LeaderLeaseSeconds != 30 || f.LeaderLease() != 30*time.Second {
		t.Fatalf("lease = %d (%v)", f.LeaderLeaseSeconds, f.LeaderLease())
	}
	if f.Interval() != time.Hour {
		t.Fatalf("Interval() = %v", f.Interval())
	}
}

func TestFleetLearningValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*FleetLearningConfig)
		key  string
	}{
		{"interval low", func(f *FleetLearningConfig) { f.IntervalMinutes = 4 },
			"fleet_learning.interval_minutes"},
		{"interval high", func(f *FleetLearningConfig) { f.IntervalMinutes = 1441 },
			"fleet_learning.interval_minutes"},
		{"similarity zero", func(f *FleetLearningConfig) { f.LookalikeMinSimilarity = 0 },
			"fleet_learning.lookalike_min_similarity"},
		{"similarity over one", func(f *FleetLearningConfig) { f.LookalikeMinSimilarity = 1.01 },
			"fleet_learning.lookalike_min_similarity"},
		{"prior outcomes", func(f *FleetLearningConfig) { f.MinPriorOutcomes = 0 },
			"fleet_learning.min_prior_outcomes"},
		{"finding databases", func(f *FleetLearningConfig) { f.FleetFindingMinDatabases = 1 },
			"fleet_learning.fleet_finding_min_databases"},
		{"split", func(f *FleetLearningConfig) { f.BudgetSplit = "random" },
			"fleet_learning.budget_split"},
		{"floor negative", func(f *FleetLearningConfig) { f.BudgetFloorPct = -1 },
			"fleet_learning.budget_floor_pct"},
		{"floor over 100", func(f *FleetLearningConfig) { f.BudgetFloorPct = 101 },
			"fleet_learning.budget_floor_pct"},
		{"ceiling under 100", func(f *FleetLearningConfig) { f.BudgetCeilingPct = 99 },
			"fleet_learning.budget_ceiling_pct"},
		{"ceiling over 1000", func(f *FleetLearningConfig) { f.BudgetCeilingPct = 1001 },
			"fleet_learning.budget_ceiling_pct"},
		{"lease too short", func(f *FleetLearningConfig) { f.LeaderLeaseSeconds = 9 },
			"fleet_learning.leader_lease_seconds"},
		{"lease too long", func(f *FleetLearningConfig) { f.LeaderLeaseSeconds = 601 },
			"fleet_learning.leader_lease_seconds"},
		{"lease negative", func(f *FleetLearningConfig) { f.LeaderLeaseSeconds = -1 },
			"fleet_learning.leader_lease_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.mut(&c.FleetLearning)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err = %v, want one naming %s", err, tc.key)
			}
		})
	}
}

func TestFleetLearningValidBoundaries(t *testing.T) {
	c := DefaultConfig()
	c.FleetLearning.IntervalMinutes = 5
	c.FleetLearning.LookalikeMinSimilarity = 1
	c.FleetLearning.MinPriorOutcomes = 1
	c.FleetLearning.FleetFindingMinDatabases = 2
	c.FleetLearning.BudgetSplit = "even"
	c.FleetLearning.BudgetFloorPct = 0
	c.FleetLearning.BudgetCeilingPct = 100
	c.FleetLearning.LeaderLeaseSeconds = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("lower boundaries rejected: %v", err)
	}
	c.FleetLearning.IntervalMinutes = 1440
	c.FleetLearning.BudgetFloorPct = 100
	c.FleetLearning.BudgetCeilingPct = 1000
	c.FleetLearning.LeaderLeaseSeconds = 600
	if err := c.Validate(); err != nil {
		t.Fatalf("upper boundaries rejected: %v", err)
	}
	if c.FleetLearning.LeaderLease() != 600*time.Second {
		t.Fatal("lease duration")
	}
	c.FleetLearning.LeaderLeaseSeconds = 10
	if err := c.Validate(); err != nil {
		t.Fatalf("lease 10 rejected: %v", err)
	}
}

func TestFleetLearningLoadsFromYAML(t *testing.T) {
	path := wave5WriteYAML(t, "fleet_learning:\n  enabled: false\n  include_names: true\n"+
		"  budget_split: even\n  leader_lease_seconds: 0\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	f := c.FleetLearning
	if f.Enabled || !f.IncludeNames || f.BudgetSplit != "even" || f.LeaderLeaseSeconds != 0 {
		t.Fatalf("loaded %+v", f)
	}
	if f.IntervalMinutes != 60 || f.BudgetFloorPct != 50 {
		t.Fatalf("unset keys must keep their defaults: %+v", f)
	}
	if f.LeaderLease() != 0 {
		t.Fatal("lease 0 disables election")
	}
}

func TestFleetLearningKeysAreOperatorPreferences(t *testing.T) {
	for _, key := range []string{"fleet_learning.enabled", "fleet_learning.include_names",
		"fleet_learning.interval_minutes", "fleet_learning.lookalike_min_similarity",
		"fleet_learning.min_prior_outcomes", "fleet_learning.fleet_finding_min_databases",
		"fleet_learning.budget_split", "fleet_learning.budget_floor_pct",
		"fleet_learning.budget_ceiling_pct", "fleet_learning.leader_lease_seconds"} {
		class, ok := KeyClassOf(key)
		if !ok || class != KeyOperatorPreference {
			t.Errorf("%s class = %q (known=%v), want operator_preference", key, class, ok)
		}
	}
}
