package main

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// config-01 review: one YAML edit that adds a database and lowers a trust
// ceiling must start the new database under the lowered ceiling. The fleet
// owner builds the new runtime during Prepare, before the trust owner
// publishes the ceiling, so the runtime's config clone still holds the old
// value unless the candidate is seeded from the desired config.

func TestFleetReloadAddedDatabaseStartsUnderLoweredCeiling(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	if !fleetMgr.GetInstance("b").Executor.TrustCeiling().Tier3Safe {
		t.Fatal("fixture must start with tier3_safe on")
	}
	extra := testdbExtra(t, env, "c")
	if err := env.reload(func(c *config.Config) {
		c.Databases = append(c.Databases, extra)
		c.Trust.Tier3Safe = false
		c.Trust.MaintenanceWindow = "never"
		c.Trust.RollbackCooldownDays = 13
	}); err != nil {
		t.Fatalf("add+lower reload: %v", err)
	}
	for _, name := range []string{"ctl", "b", "c"} {
		inst := fleetMgr.GetInstance(name)
		if inst == nil || inst.Executor == nil {
			t.Fatalf("db %q has no running executor after the reload", name)
		}
		got := inst.Executor.TrustCeiling()
		if got.Tier3Safe || got.MaintenanceWindow != "never" ||
			got.RollbackCooldownDays != 13 {
			t.Errorf("db %q ceiling = %+v, want tier3_safe off, window never, "+
				"cooldown 13", name, got)
		}
	}
}

func TestFleetReloadRebuiltDatabaseKeepsLoweredCeiling(t *testing.T) {
	newFleetReloadEnv(t, "ctl", "b")
	old := fleetMgr.GetInstance("b")
	if err := (&fleetReloadEnv{}).reload(func(c *config.Config) {
		idx := databaseIndex(c.Databases, "b")
		c.Databases[idx].MaxConnections = 4 // restart-class: rebuilds "b"
		c.Trust.Tier3Safe = false
		c.Trust.RollbackThresholdPct = 3
	}); err != nil {
		t.Fatalf("rebuild+lower reload: %v", err)
	}
	inst := fleetMgr.GetInstance("b")
	if inst == nil || inst == old || inst.Executor == nil {
		t.Fatalf("db b was not rebuilt: old=%p new=%p", old, inst)
	}
	if got := inst.Executor.TrustCeiling(); got.Tier3Safe || got.RollbackThresholdPct != 3 {
		t.Fatalf("rebuilt db b ceiling = %+v, want tier3_safe off, threshold 3", got)
	}
}

func candidateReload(cfg *config.Config, names ...string) *preparedFleetReload {
	p := &preparedFleetReload{}
	for _, name := range names {
		exec := executor.New(nil, config.Clone(cfg), time.Time{}, nil)
		p.candidates = append(p.candidates, fleetCandidate{
			instance: &fleet.DatabaseInstance{Name: name, Executor: exec}})
	}
	p.candidates = append(p.candidates, fleetCandidate{
		instance: &fleet.DatabaseInstance{Name: "failed"}}) // placeholder
	return p
}

func TestSeedCeilingsHoldsARaiseBackUntilCommit(t *testing.T) {
	active := ceilingStartConfig()
	active.Trust.Tier3Moderate = false
	desired := config.Clone(active)
	desired.Trust.Tier3Moderate, desired.Trust.Tier3Safe = true, false
	desired.Trust.RollbackCooldownDays = 2
	p := candidateReload(active, "c")
	p.ceiling = changedTrustCeiling(active, desired)
	if p.ceiling == nil {
		t.Fatal("a moved ceiling was reported unchanged")
	}
	exec := p.candidates[0].instance.Executor
	p.seedCeilings(false)
	got := exec.TrustCeiling()
	if got.Tier3Moderate || got.Tier3Safe || got.RollbackCooldownDays != 7 {
		t.Fatalf("pre-commit ceiling = %+v, want the stricter of old and new", got)
	}
	p.seedCeilings(true)
	if got := exec.TrustCeiling(); got != executor.TrustCeilingFromConfig(desired.Trust) {
		t.Fatalf("committed ceiling = %+v, want the desired one", got)
	}
}

func TestSeedCeilingsLeavesAnUnchangedCeilingAlone(t *testing.T) {
	active := ceilingStartConfig()
	if got := changedTrustCeiling(active, config.Clone(active)); got != nil {
		t.Fatalf("unchanged ceiling = %+v, want nil", got)
	}
	if changedTrustCeiling(nil, active) != nil || changedTrustCeiling(active, nil) != nil {
		t.Fatal("a nil config must not produce a ceiling")
	}
	// A failed commit left the process stricter than the active config; a
	// reload that does not move the ceiling must not raise a new runtime.
	stricter := config.Clone(active)
	stricter.Trust.Tier3Safe = false
	p := candidateReload(stricter, "c")
	p.seedCeilings(false)
	p.seedCeilings(true)
	if p.candidates[0].instance.Executor.TrustCeiling().Tier3Safe {
		t.Fatal("an unchanged reload raised a new runtime past the process ceiling")
	}
}
