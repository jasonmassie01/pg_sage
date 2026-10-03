package config

import (
	"strings"
	"testing"
)

// Perf storage phase: query_store keeps its own window (default 14 days;
// its readers look back at most 7) instead of following snapshots_days
// (90), and snapshots are capped at retention.snapshots_max_pct of the
// database (default 5, half the sage_footprint warning).

func TestStorageRetention_DefaultsWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Retention.QueryStoreDays != 14 || DefaultRetentionQueryStoreDays != 14 {
		t.Fatalf("query_store_days = %d (const %d), want 14", cfg.Retention.QueryStoreDays,
			DefaultRetentionQueryStoreDays)
	}
	if cfg.Retention.SnapshotsMaxPct != 5 || DefaultRetentionSnapshotsMaxPct != 5 {
		t.Fatalf("snapshots_max_pct = %d (const %d), want 5", cfg.Retention.SnapshotsMaxPct,
			DefaultRetentionSnapshotsMaxPct)
	}
	d := DefaultConfig().Retention
	if d.QueryStoreDays != 14 || d.SnapshotsMaxPct != 5 {
		t.Fatalf("DefaultConfig disagrees with Load(nil): %+v", d)
	}
}

func TestStorageRetention_PartialSectionKeepsDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t, "retention:\n  snapshots_days: 30\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r := cfg.Retention
	if r.QueryStoreDays != 14 || r.SnapshotsMaxPct != 5 || r.SnapshotsDays != 30 {
		t.Fatalf("retention = %+v, want the new defaults kept", r)
	}
}

func TestStorageRetention_ExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		yaml       string
		days, pctg int
	}{
		{"query_store_days: 0\n  snapshots_max_pct: 0", 0, 0},
		{"query_store_days: 1\n  snapshots_max_pct: 1", 1, 1},
		{"query_store_days: 3650\n  snapshots_max_pct: 100", 3650, 100},
	} {
		cfg, err := loadRCAYAML(t, "retention:\n  "+tc.yaml+"\n")
		if err != nil || cfg.Retention.QueryStoreDays != tc.days ||
			cfg.Retention.SnapshotsMaxPct != tc.pctg {
			t.Errorf("%q: got %+v (%v)", tc.yaml, cfg.Retention, err)
		}
	}
}

func TestStorageRetention_OutOfRangeRefused(t *testing.T) {
	for yaml, key := range map[string]string{
		"query_store_days: -1":   "retention.query_store_days",
		"query_store_days: 3651": "retention.query_store_days",
		"snapshots_max_pct: -1":  "retention.snapshots_max_pct",
		"snapshots_max_pct: 101": "retention.snapshots_max_pct",
	} {
		_, err := loadRCAYAML(t, "retention:\n  "+yaml+"\n")
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v, want a refusal naming %s", yaml, err, key)
		}
	}
}
