package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// config-01: trust ceilings (tier3_safe, tier3_moderate, maintenance_window,
// rollback_*, cascade_cooldown_cycles) apply live through the trust_policy
// owner to every fleet executor; a failed commit never raises autonomy.

type ceilingFleet struct {
	mgr   *fleet.DatabaseManager
	execs map[string]*executor.Executor
	names []string
}

func ceilingStartConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "autonomous"
	cfg.Trust.Tier3Safe, cfg.Trust.Tier3Moderate = true, true
	cfg.Trust.MaintenanceWindow = "always"
	return cfg
}

// newCeilingFleet registers one executor per name, each with its own derived
// config clone as fleet mode builds them; "pinned" keeps an explicit level.
func newCeilingFleet(names ...string) *ceilingFleet {
	base := ceilingStartConfig()
	f := &ceilingFleet{mgr: fleet.NewManager(base),
		execs: map[string]*executor.Executor{}, names: names}
	for _, name := range names {
		exec := executor.New(nil, config.Clone(base), time.Time{}, nil)
		dbCfg := config.DatabaseConfig{Name: name}
		if name == "pinned" {
			_ = exec.SetTrustLevel("advisory")
			dbCfg.TrustLevel, dbCfg.TrustLevelExplicit = "advisory", true
		}
		f.execs[name] = exec
		f.mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Executor: exec,
			Config: dbCfg, Status: &fleet.InstanceStatus{TrustLevel: exec.TrustLevel()}})
	}
	return f
}

func (f *ceilingFleet) owner() *trustPolicyOwner {
	return &trustPolicyOwner{manager: func() *fleet.DatabaseManager { return f.mgr }}
}

func applyCeiling(
	t *testing.T, controller *config.ConfigController, set func(*config.TrustConfig),
) config.ApplyResult {
	t.Helper()
	candidate := controller.Desired().Config
	set(&candidate.Trust)
	result, err := controller.Apply(context.Background(),
		controller.Desired().Generation, candidate)
	if err != nil {
		t.Fatalf("apply ceiling: %v", err)
	}
	return result
}

func TestTrustCeilingAppliesLiveToEveryFleetExecutor(t *testing.T) {
	f := newCeilingFleet("a", "b", "pinned")
	controller := config.NewConfigController(ceilingStartConfig(), nil, f.owner())
	result := applyCeiling(t, controller, func(c *config.TrustConfig) { c.Tier3Safe = false })
	if len(result.Applied) != 1 || result.Applied[0] != "trust.tier3_safe" ||
		len(result.PendingRestart) != 0 {
		t.Fatalf("result applied=%v pending=%v, want [trust.tier3_safe] and none",
			result.Applied, result.PendingRestart)
	}
	if result.ComponentStatus["trust_policy"] != "applied" {
		t.Fatalf("component status = %v", result.ComponentStatus)
	}
	for _, name := range f.names {
		ceiling := f.execs[name].TrustCeiling()
		if ceiling.Tier3Safe || !ceiling.Tier3Moderate || ceiling.MaintenanceWindow != "always" {
			t.Errorf("%s ceiling = %+v, want only tier3_safe off", name, ceiling)
		}
	}
	if got := f.execs["pinned"].TrustLevel(); got != "advisory" {
		t.Fatalf("pinned database trust = %q, want its own advisory kept", got)
	}
	if got := f.execs["a"].TrustLevel(); got != "autonomous" {
		t.Fatalf("inherited database trust = %q, want autonomous untouched", got)
	}
}

func TestTrustCeilingNarrowedWindowAndCooldownsReachExecutors(t *testing.T) {
	f := newCeilingFleet("a", "b")
	controller := config.NewConfigController(ceilingStartConfig(), nil, f.owner())
	applyCeiling(t, controller, func(c *config.TrustConfig) {
		c.MaintenanceWindow = "never"
		c.RollbackCooldownDays, c.CascadeCooldownCycles = 9, 4
		c.RollbackThresholdPct, c.RollbackWindowMinutes = 12, 45
	})
	for _, name := range f.names {
		got := f.execs[name].TrustCeiling()
		want := executor.TrustCeiling{Tier3Safe: true, Tier3Moderate: true,
			MaintenanceWindow: "never", RollbackThresholdPct: 12,
			RollbackWindowMinutes: 45, RollbackCooldownDays: 9, CascadeCooldownCycles: 4}
		if got != want {
			t.Errorf("%s ceiling = %+v, want %+v", name, got, want)
		}
	}
}

func TestTrustCeilingCommitFailureKeepsTheLowerCeilingEverywhere(t *testing.T) {
	f := newCeilingFleet("a", "b", "c")
	owner := f.owner()
	var mu sync.Mutex
	failed := false
	owner.ceiling = func(inst *fleet.DatabaseInstance, c executor.TrustCeiling) error {
		mu.Lock()
		defer mu.Unlock()
		if inst.Name == "b" && !failed {
			failed = true
			return errors.New("injected ceiling failure")
		}
		inst.Executor.SetTrustCeiling(&c)
		return nil
	}
	controller := config.NewConfigController(ceilingStartConfig(), nil, owner)
	result := applyCeiling(t, controller, func(c *config.TrustConfig) {
		c.Tier3Safe = false
		c.MaintenanceWindow = "never"
	})
	if result.ComponentStatus["trust_policy"] != "commit_failed" || len(result.Applied) != 0 {
		t.Fatalf("result = %+v, want commit_failed with nothing applied", result)
	}
	if !containsString(result.PendingRestart, "trust.tier3_safe") {
		t.Fatalf("pending = %v, want trust.tier3_safe reported unapplied", result.PendingRestart)
	}
	for _, name := range f.names {
		got := f.execs[name].TrustCeiling()
		if got.Tier3Safe {
			t.Errorf("%s kept tier3_safe on after a failed downgrade", name)
		}
		if got.MaintenanceWindow != "always" {
			t.Errorf("%s window = %q, want the old window after a failed commit",
				name, got.MaintenanceWindow)
		}
	}
}

func TestTrustCeilingFailedRaiseNeverRaisesAutonomy(t *testing.T) {
	start := ceilingStartConfig()
	start.Trust.Tier3Moderate = false
	f := newCeilingFleet("a", "b")
	for _, exec := range f.execs {
		ceiling := exec.TrustCeiling()
		ceiling.Tier3Moderate = false
		exec.SetTrustCeiling(&ceiling)
	}
	owner := f.owner()
	owner.ceiling = func(inst *fleet.DatabaseInstance, c executor.TrustCeiling) error {
		if inst.Name == "b" && c.Tier3Moderate {
			return errors.New("injected raise failure")
		}
		inst.Executor.SetTrustCeiling(&c)
		return nil
	}
	controller := config.NewConfigController(start, nil, owner)
	result := applyCeiling(t, controller, func(c *config.TrustConfig) { c.Tier3Moderate = true })
	if result.ComponentStatus["trust_policy"] != "commit_failed" {
		t.Fatalf("result = %+v, want commit_failed", result)
	}
	for _, name := range f.names {
		if f.execs[name].TrustCeiling().Tier3Moderate {
			t.Errorf("%s was raised by a failed commit", name)
		}
	}
}

func TestTrustCeilingRaiseIsLiveWithWarning(t *testing.T) {
	start := ceilingStartConfig()
	start.Trust.Tier3Moderate = false
	f := newCeilingFleet("a")
	ceiling := f.execs["a"].TrustCeiling()
	ceiling.Tier3Moderate = false
	f.execs["a"].SetTrustCeiling(&ceiling)
	controller := config.NewConfigController(start, nil, f.owner())
	result := applyCeiling(t, controller, func(c *config.TrustConfig) { c.Tier3Moderate = true })
	if !f.execs["a"].TrustCeiling().Tier3Moderate {
		t.Fatal("tier3_moderate raise did not reach the executor")
	}
	joined := strings.Join(result.Warnings, "\n")
	if !strings.Contains(joined, "ceiling raised") ||
		!strings.Contains(joined, "trust.tier3_moderate") ||
		!strings.Contains(joined, "next cycle") {
		t.Fatalf("warnings = %v, want a ceiling-raised note naming the field", result.Warnings)
	}
	lowered := applyCeiling(t, controller, func(c *config.TrustConfig) { c.Tier3Safe = false })
	if strings.Contains(strings.Join(lowered.Warnings, "\n"), "ceiling raised") {
		t.Fatalf("a downgrade reported a raise: %v", lowered.Warnings)
	}
}

func TestTrustCeilingPublishesToProcessConfigForLaterExecutors(t *testing.T) {
	f := newCeilingFleet("a")
	owner := f.owner()
	var published []executor.TrustCeiling
	owner.publish = func(c executor.TrustCeiling) { published = append(published, c) }
	controller := config.NewConfigController(ceilingStartConfig(), nil, owner)
	applyCeiling(t, controller, func(c *config.TrustConfig) { c.RollbackCooldownDays = 11 })
	if len(published) != 1 || published[0].RollbackCooldownDays != 11 ||
		!published[0].Tier3Safe {
		t.Fatalf("published = %+v, want one ceiling with cooldown 11", published)
	}
}

func TestPublishTrustCeilingWritesOnlyCeilingFields(t *testing.T) {
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	cfg = ceilingStartConfig()
	cfg.Trust.Tier3HighRisk = true
	publishTrustCeiling(executor.TrustCeiling{MaintenanceWindow: "never",
		RollbackThresholdPct: 7, RollbackWindowMinutes: 8, RollbackCooldownDays: 9,
		CascadeCooldownCycles: 10})
	got := cfg.Trust
	if got.Tier3Safe || got.Tier3Moderate || got.MaintenanceWindow != "never" ||
		got.RollbackThresholdPct != 7 || got.RollbackWindowMinutes != 8 ||
		got.RollbackCooldownDays != 9 || got.CascadeCooldownCycles != 10 {
		t.Fatalf("published trust = %+v", got)
	}
	if got.Level != "autonomous" || !got.Tier3HighRisk {
		t.Fatalf("publish touched non-ceiling fields: %+v", got)
	}
	cfg = nil
	publishTrustCeiling(executor.TrustCeiling{}) // nil process config: no panic
}

func TestTrustCeilingPrepareWithNilManagerChangesNothing(t *testing.T) {
	published := 0
	owner := &trustPolicyOwner{
		manager: func() *fleet.DatabaseManager { return nil },
		publish: func(executor.TrustCeiling) { published++ },
	}
	active := config.ConfigSnapshot{Generation: 1, Config: ceilingStartConfig()}
	desired := config.ConfigSnapshot{Generation: 2, Config: ceilingStartConfig()}
	desired.Config.Trust.Tier3Safe = false
	prepared, err := owner.Prepare(context.Background(), active, desired)
	if err != nil || prepared == nil {
		t.Fatalf("prepare = %v, %v; want a no-op change", prepared, err)
	}
	if err := prepared.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := prepared.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if published != 0 {
		t.Fatalf("a nil fleet published %d ceilings", published)
	}
}

func TestTrustCeilingRollbackBeforeCommitLeavesExecutorsAlone(t *testing.T) {
	f := newCeilingFleet("a")
	owner := f.owner()
	active := config.ConfigSnapshot{Generation: 1, Config: ceilingStartConfig()}
	desired := config.ConfigSnapshot{Generation: 2, Config: ceilingStartConfig()}
	desired.Config.Trust.Tier3Safe = false
	prepared, err := owner.Prepare(context.Background(), active, desired)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Persistence failed: the controller rolls back an uncommitted change.
	if err := prepared.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !f.execs["a"].TrustCeiling().Tier3Safe {
		t.Fatal("an uncommitted change lowered the executor's ceiling")
	}
}

func TestTrustCeilingOnlyChangeLeavesMetaTrustRowsAlone(t *testing.T) {
	mgr, execs := metaTrustManager(t, map[string]string{"x": "advisory", "y": "autonomous"})
	var persisted []trustPersistCall
	owner := &trustPolicyOwner{
		manager: func() *fleet.DatabaseManager { return mgr },
		persist: func(_ context.Context, inst *fleet.DatabaseInstance, level string) error {
			persisted = append(persisted, trustPersistCall{database: inst.Name, level: level})
			return nil
		},
	}
	start := config.DefaultConfig()
	start.Trust.Level = "advisory"
	controller := config.NewConfigController(start, nil, owner)
	applyCeiling(t, controller, func(c *config.TrustConfig) { c.Tier3Safe = !c.Tier3Safe })
	if len(persisted) != 0 {
		t.Fatalf("a ceiling-only change persisted trust rows: %+v", persisted)
	}
	if got := execs["y"].TrustLevel(); got != "autonomous" {
		t.Fatalf("y trust = %q, want its own autonomous row kept", got)
	}
	if got := execs["x"].TrustCeiling().Tier3Safe; got == start.Trust.Tier3Safe {
		t.Fatal("meta-mode executor did not receive the ceiling")
	}
}

func TestTrustCeilingSkipsInstancesWithoutExecutor(t *testing.T) {
	f := newCeilingFleet("a")
	f.mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "failed",
		Status: &fleet.InstanceStatus{}})
	controller := config.NewConfigController(ceilingStartConfig(), nil, f.owner())
	result := applyCeiling(t, controller, func(c *config.TrustConfig) { c.Tier3Safe = false })
	if result.ComponentStatus["trust_policy"] != "applied" ||
		f.execs["a"].TrustCeiling().Tier3Safe {
		t.Fatalf("result = %+v, want applied to the running executor", result)
	}
}

func TestTrustStartupLogNamesTheLiveCeilings(t *testing.T) {
	var info, warn lineCapture
	logTrustMeaning(ceilingStartConfig(), info.log, warn.log)
	all := strings.Join(info.lines, "\n")
	for _, want := range []string{"trust.tier3_safe", "trust.tier3_moderate",
		"trust.maintenance_window", "trust.rollback_threshold_pct",
		"trust.rollback_window_minutes", "trust.rollback_cooldown_days",
		"trust.cascade_cooldown_cycles", "live"} {
		if !strings.Contains(all, want) {
			t.Errorf("startup trust message omits %q:\n%s", want, all)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
