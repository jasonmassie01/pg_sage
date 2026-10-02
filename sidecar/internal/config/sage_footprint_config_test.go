package config

import (
	"strings"
	"testing"
)

// retention.sage_size_warning_pct: the share of the database that sage's
// own tables may take before the analyzer raises a finding. Default 10,
// 0 disables, out-of-range values are refused at load.

func TestSageSizeWarningPct_DefaultWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retention.SageSizeWarningPct != 10 || DefaultRetentionSageSizeWarningPct != 10 {
		t.Fatalf("default = %d (const %d), want 10", cfg.Retention.SageSizeWarningPct,
			DefaultRetentionSageSizeWarningPct)
	}
	if DefaultConfig().Retention.SageSizeWarningPct != 10 {
		t.Fatal("DefaultConfig disagrees with Load(nil)")
	}
}

func TestSageSizeWarningPct_PartialSectionKeepsDefault(t *testing.T) {
	cfg, err := loadRCAYAML(t, "retention:\n  snapshots_days: 30\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retention.SageSizeWarningPct != 10 || cfg.Retention.SnapshotsDays != 30 {
		t.Fatalf("retention = %+v, want the default share kept", cfg.Retention)
	}
}

func TestSageSizeWarningPct_ExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int
	}{{"0", 0}, {"1", 1}, {"25", 25}, {"100", 100}} {
		cfg, err := loadRCAYAML(t, "retention:\n  sage_size_warning_pct: "+tc.yaml+"\n")
		if err != nil || cfg.Retention.SageSizeWarningPct != tc.want {
			t.Errorf("%s: got %+v (%v), want %d", tc.yaml, cfg, err, tc.want)
		}
	}
}

func TestSageSizeWarningPct_OutOfRangeRefused(t *testing.T) {
	for _, v := range []string{"-1", "101"} {
		_, err := loadRCAYAML(t, "retention:\n  sage_size_warning_pct: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "retention.sage_size_warning_pct") {
			t.Errorf("%s: err = %v, want a refusal naming the key", v, err)
		}
	}
}
