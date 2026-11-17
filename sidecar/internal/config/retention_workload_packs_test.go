package config

import (
	"strings"
	"testing"
)

// evid-02a: stored workload evidence packs are kept
// retention.workload_packs_days (default 180), 0-3650 with 0 disabling the
// purge, validated like the rollup windows. No concurrent access tests:
// validation is a pure function of the values.

func TestWorkloadPacksRetention_DefaultWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retention.WorkloadPacksDays != 180 || DefaultRetentionWorkloadPacksDays != 180 {
		t.Fatalf("workload_packs_days = %d (const %d), want 180",
			cfg.Retention.WorkloadPacksDays, DefaultRetentionWorkloadPacksDays)
	}
	if DefaultConfig().Retention.WorkloadPacksDays != 180 {
		t.Fatal("DefaultConfig disagrees with Load(nil)")
	}
}

func TestWorkloadPacksRetention_OutOfRangeNamesTheKey(t *testing.T) {
	for _, v := range []string{"-1", "3651"} {
		_, err := loadRCAYAML(t, "retention:\n  workload_packs_days: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "retention.workload_packs_days") ||
			!strings.Contains(err.Error(), "0-3650") {
			t.Fatalf("workload_packs_days %s: %v, want the key and its range", v, err)
		}
	}
}

func TestWorkloadPacksRetention_BoundsAndZeroLoad(t *testing.T) {
	for _, v := range []string{"0", "1", "3650"} {
		cfg, err := loadRCAYAML(t, "retention:\n  workload_packs_days: "+v+"\n")
		if err != nil {
			t.Fatalf("workload_packs_days %s: %v", v, err)
		}
		if got := cfg.Retention.WorkloadPacksDays; v == "0" && got != 0 ||
			v == "3650" && got != 3650 {
			t.Fatalf("workload_packs_days %s loaded as %d", v, got)
		}
	}
}
