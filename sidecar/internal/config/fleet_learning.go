package config

import (
	"fmt"
	"time"
)

// FleetLearningConfig configures fleet learning (roadmap phase 3): schema
// fingerprints, look-alike priors, fleet findings, the need-based split of
// llm.fleet_token_budget_daily and leader election among sidecars sharing
// a control database. Every key is an operator preference: none widens
// what pg_sage may do or spend.
type FleetLearningConfig struct {
	Enabled                  bool    `yaml:"enabled" doc:"Fingerprint every fleet database's schema and workload shape, show look-alike evidence on proposals and recurring fleet findings. Evidence only: never raises trust or confidence. Default: true."`
	IncludeNames             bool    `yaml:"include_names" doc:"Also store table names with fingerprints (for drill-down). Off by default: fingerprints hold only hashes of shapes, never names, data or literals."`
	IntervalMinutes          int     `yaml:"interval_minutes" doc:"Minutes between fingerprint and outcome-digest cycles. 5-1440. Default: 60."`
	LookalikeMinSimilarity   float64 `yaml:"lookalike_min_similarity" doc:"Similarity (0-1, weighted overlap of table, index and query shapes) at which another database of the same fleet and tenant counts as a look-alike. Default: 0.6."`
	MinPriorOutcomes         int     `yaml:"min_prior_outcomes" doc:"Verified outcomes on look-alike databases needed before their evidence is shown on a proposal. 1-1000. Default: 3."`
	FleetFindingMinDatabases int     `yaml:"fleet_finding_min_databases" doc:"Databases on which the same problem must be open to show it as one fleet finding. 2-10000. Default: 3."`
	BudgetSplit              string  `yaml:"budget_split" doc:"How llm.fleet_token_budget_daily is split: need (by open findings and incidents, within the floor and ceiling) or even. The cap itself is never exceeded. Default: need."`
	BudgetFloorPct           int     `yaml:"budget_floor_pct" doc:"Smallest share of a database under the need split, as a percentage of the even share. 0-100. Default: 50."`
	BudgetCeilingPct         int     `yaml:"budget_ceiling_pct" doc:"Largest share of a database under the need split, as a percentage of the even share. 100-1000. Default: 300."`
	LeaderLeaseSeconds       int     `yaml:"leader_lease_seconds" doc:"Lease of the leader sidecar that runs fleet-wide jobs when several sidecars share a control database. 10-600; 0 disables election (every sidecar runs them). Default: 30."`
	ControlDatabase          string  `yaml:"control_database" doc:"YAML fleet database that holds the leader lease; pin it to run several sidecars on one fleet. While it is down every sidecar follows, never leading elsewhere. Not with meta_db. Default: empty."`
	OrphanScopeDays          int     `yaml:"orphan_scope_days" doc:"Days after its leader lease expired that another fleet scope's fingerprints, digests and lease are deleted (a renamed or retired fleet). 1-3650. Default: 14."`
}

// Fleet learning defaults.
const (
	DefaultFleetLearningIntervalMinutes = 60
	DefaultLookalikeMinSimilarity       = 0.6
	DefaultMinPriorOutcomes             = 3
	DefaultFleetFindingMinDatabases     = 3
	DefaultFleetBudgetSplit             = "need"
	DefaultFleetBudgetFloorPct          = 50
	DefaultFleetBudgetCeilingPct        = 300
	DefaultLeaderLeaseSeconds           = 30
	DefaultOrphanScopeDays              = 14
)

func defaultFleetLearningConfig() FleetLearningConfig {
	return FleetLearningConfig{Enabled: true,
		IntervalMinutes:          DefaultFleetLearningIntervalMinutes,
		LookalikeMinSimilarity:   DefaultLookalikeMinSimilarity,
		MinPriorOutcomes:         DefaultMinPriorOutcomes,
		FleetFindingMinDatabases: DefaultFleetFindingMinDatabases,
		BudgetSplit:              DefaultFleetBudgetSplit,
		BudgetFloorPct:           DefaultFleetBudgetFloorPct,
		BudgetCeilingPct:         DefaultFleetBudgetCeilingPct,
		LeaderLeaseSeconds:       DefaultLeaderLeaseSeconds,
		OrphanScopeDays:          DefaultOrphanScopeDays}
}

// Interval is the fingerprint cycle period.
func (f FleetLearningConfig) Interval() time.Duration {
	return time.Duration(f.IntervalMinutes) * time.Minute
}

// OrphanScopeAge is how long after its lease expired a scope is pruned.
func (f FleetLearningConfig) OrphanScopeAge() time.Duration {
	return time.Duration(f.OrphanScopeDays) * 24 * time.Hour
}

// LeaderLease is the leader lease; 0 disables election.
func (f FleetLearningConfig) LeaderLease() time.Duration {
	return time.Duration(f.LeaderLeaseSeconds) * time.Second
}

// validateFleetLearning checks fleet_learning, including that a pinned
// control database is one of databases: (a meta-DB fleet keeps its
// control in meta_db).
func (c *Config) validateFleetLearning() error {
	if err := c.FleetLearning.validate(); err != nil {
		return err
	}
	pin := c.FleetLearning.ControlDatabase
	if pin == "" {
		return nil
	}
	if c.HasMetaDB() {
		return fmt.Errorf("fleet_learning.control_database %q applies to YAML fleets; "+
			"with meta_db the meta database is the control database", pin)
	}
	for _, d := range c.Databases {
		if d.Name == pin {
			return nil
		}
	}
	return fmt.Errorf("fleet_learning.control_database %q is not a name under "+
		"the databases key", pin)
}

func (f FleetLearningConfig) validate() error {
	ints := []struct {
		key      string
		v, lo, h int
	}{
		{"interval_minutes", f.IntervalMinutes, 5, 1440},
		{"min_prior_outcomes", f.MinPriorOutcomes, 1, 1000},
		{"fleet_finding_min_databases", f.FleetFindingMinDatabases, 2, 10000},
		{"budget_floor_pct", f.BudgetFloorPct, 0, 100},
		{"budget_ceiling_pct", f.BudgetCeilingPct, 100, 1000},
		{"orphan_scope_days", f.OrphanScopeDays, 1, 3650},
	}
	for _, c := range ints {
		if c.v < c.lo || c.v > c.h {
			return fmt.Errorf("fleet_learning.%s must be %d-%d, got %d", c.key, c.lo,
				c.h, c.v)
		}
	}
	if f.LookalikeMinSimilarity <= 0 || f.LookalikeMinSimilarity > 1 {
		return fmt.Errorf("fleet_learning.lookalike_min_similarity must be in (0,1], "+
			"got %v", f.LookalikeMinSimilarity)
	}
	if f.BudgetSplit != "need" && f.BudgetSplit != "even" {
		return fmt.Errorf("fleet_learning.budget_split must be need or even, got %q",
			f.BudgetSplit)
	}
	if f.LeaderLeaseSeconds != 0 && (f.LeaderLeaseSeconds < 10 ||
		f.LeaderLeaseSeconds > 600) {
		return fmt.Errorf("fleet_learning.leader_lease_seconds must be 0 or 10-600, "+
			"got %d", f.LeaderLeaseSeconds)
	}
	return nil
}
