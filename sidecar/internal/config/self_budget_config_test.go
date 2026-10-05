package config

import (
	"strings"
	"testing"
)

// self_budget: pg_sage's declared budget for itself (roadmap phase 3,
// "cheap, self-aware observer"). CPU of the sidecar process per collector
// cycle, database time per hour, shared blocks per hour and the sage
// schema's size. 0 disables a resource; database time per hour defaults
// to 0, which keeps analyzer.self_cost_budget_ms (per cycle) as the
// database-time budget so one breach never raises two findings.

func TestSelfBudget_DefaultsWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := SelfBudgetConfig{CPUMsPerCycle: 600, DBTimeMsPerHour: 0,
		BlocksPerHour: 18_000_000, StorageMB: 10240}
	if cfg.SelfBudget != want {
		t.Fatalf("self_budget = %+v, want %+v", cfg.SelfBudget, want)
	}
	if DefaultConfig().SelfBudget != want || DefaultSelfBudget() != want {
		t.Fatal("DefaultConfig / DefaultSelfBudget disagree with Load(nil)")
	}
}

func TestSelfBudget_PartialSectionKeepsOtherDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t, "self_budget:\n  storage_mb: 512\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SelfBudget.StorageMB != 512 || cfg.SelfBudget.CPUMsPerCycle != 600 ||
		cfg.SelfBudget.BlocksPerHour != 18_000_000 {
		t.Fatalf("self_budget = %+v, want storage 512 and the other defaults", cfg.SelfBudget)
	}
}

func TestSelfBudget_ZeroDisablesEachResource(t *testing.T) {
	cfg, err := loadRCAYAML(t, "self_budget:\n  cpu_ms_per_cycle: 0\n"+
		"  db_time_ms_per_hour: 0\n  blocks_per_hour: 0\n  storage_mb: 0\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SelfBudget != (SelfBudgetConfig{}) {
		t.Fatalf("self_budget = %+v, want every resource disabled", cfg.SelfBudget)
	}
}

func TestSelfBudget_BoundsAndRefusals(t *testing.T) {
	ok := map[string]string{
		"cpu_ms_per_cycle":    "3600000",
		"db_time_ms_per_hour": "3600000",
		"blocks_per_hour":     "1000000000000",
		"storage_mb":          "10485760",
	}
	for key, v := range ok {
		if _, err := loadRCAYAML(t, "self_budget:\n  "+key+": "+v+"\n"); err != nil {
			t.Errorf("%s=%s refused: %v", key, v, err)
		}
	}
	bad := map[string][]string{
		"cpu_ms_per_cycle":    {"-1", "3600001"},
		"db_time_ms_per_hour": {"-1", "3600001"},
		"blocks_per_hour":     {"-1", "1000000000001"},
		"storage_mb":          {"-1", "10485761"},
	}
	for key, values := range bad {
		for _, v := range values {
			_, err := loadRCAYAML(t, "self_budget:\n  "+key+": "+v+"\n")
			if err == nil || !strings.Contains(err.Error(), "self_budget."+key) {
				t.Errorf("%s=%s: err = %v, want a refusal naming the key", key, v, err)
			}
		}
	}
}

func TestSelfBudget_UnknownKeyRefused(t *testing.T) {
	_, err := loadRCAYAML(t, "self_budget:\n  cpu_percent: 5\n")
	if err == nil || !strings.Contains(err.Error(), "cpu_percent") {
		t.Fatalf("unknown key: err = %v, want strict YAML to refuse it", err)
	}
}

// The effective database-time budget per hour: the explicit value, or
// analyzer.self_cost_budget_ms scaled from per collector cycle to per hour.
func TestSelfBudget_EffectiveDBTimePerHour(t *testing.T) {
	c := DefaultConfig()
	c.Collector.IntervalSeconds = 60
	c.Analyzer.SelfCostBudgetMs = 3000
	if got, explicit := c.EffectiveSelfDBTimeMsPerHour(); got != 180_000 || explicit {
		t.Fatalf("inherited = %v (explicit %v), want 180000 from 3000 ms x 60 cycles",
			got, explicit)
	}
	c.Collector.IntervalSeconds = 15
	if got, _ := c.EffectiveSelfDBTimeMsPerHour(); got != 720_000 {
		t.Fatalf("15 s cycles: %v, want 720000", got)
	}
	c.SelfBudget.DBTimeMsPerHour = 90_000
	if got, explicit := c.EffectiveSelfDBTimeMsPerHour(); got != 90_000 || !explicit {
		t.Fatalf("explicit = %v (%v), want 90000 explicit", got, explicit)
	}
	c.SelfBudget.DBTimeMsPerHour, c.Analyzer.SelfCostBudgetMs = 0, 0
	if got, _ := c.EffectiveSelfDBTimeMsPerHour(); got != 0 {
		t.Fatalf("both disabled = %v, want 0", got)
	}
	c.Analyzer.SelfCostBudgetMs, c.Collector.IntervalSeconds = 3000, 0
	if got, _ := c.EffectiveSelfDBTimeMsPerHour(); got != 180_000 {
		t.Fatalf("zero interval = %v, want the 60 s default cycle", got)
	}
}
