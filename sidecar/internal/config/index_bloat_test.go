package config

import (
	"strings"
	"testing"
)

// analyzer.index_bloat_threshold_pct and analyzer.index_bloat_min_bytes
// (rules-13): the index_bloat rule's threshold (default 30) and the least
// index size the collector estimates (default 100 MiB). 0 (unset) means
// the default for both; a negative size floor is refused at load.
// No concurrency test: the accessors read one immutable field each.

func TestIndexBloat_DefaultsWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Analyzer
	if a.IndexBloatThresholdPct != 30 || a.IndexBloatThreshold() != 30 {
		t.Fatalf("threshold = %d (accessor %d), want 30",
			a.IndexBloatThresholdPct, a.IndexBloatThreshold())
	}
	if DefaultIndexBloatMinBytes != 100<<20 || a.IndexBloatMinBytes != 100<<20 ||
		a.IndexBloatFloorBytes() != 100<<20 {
		t.Fatalf("floor = %d (const %d, accessor %d), want 100 MiB",
			a.IndexBloatMinBytes, DefaultIndexBloatMinBytes, a.IndexBloatFloorBytes())
	}
	d := DefaultConfig().Analyzer
	if d.IndexBloatMinBytes != a.IndexBloatMinBytes ||
		d.IndexBloatThresholdPct != a.IndexBloatThresholdPct {
		t.Fatal("DefaultConfig disagrees with Load(nil)")
	}
}

// Default-value masking: a zero-value analyzer section (no defaults
// applied) still judges at 30% and reads indexes from 100 MiB.
func TestIndexBloat_ZeroValueStructUsesDefaults(t *testing.T) {
	var a AnalyzerConfig
	if got := a.IndexBloatThreshold(); got != 30 {
		t.Errorf("zero-value threshold = %d, want 30", got)
	}
	if got := a.IndexBloatFloorBytes(); got != 100<<20 {
		t.Errorf("zero-value floor = %d, want 100 MiB", got)
	}
	a.IndexBloatThresholdPct = -5
	if got := a.IndexBloatThreshold(); got != 30 {
		t.Errorf("negative threshold = %d, want the default 30", got)
	}
}

func TestIndexBloat_ExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		yaml      string
		threshold int
		floor     int64
	}{
		{"index_bloat_threshold_pct: 0\n  index_bloat_min_bytes: 0", 30, 100 << 20},
		{"index_bloat_threshold_pct: 1\n  index_bloat_min_bytes: 1", 1, 1},
		{"index_bloat_threshold_pct: 50\n  index_bloat_min_bytes: 8192", 50, 8192},
		{"index_bloat_threshold_pct: 100\n  index_bloat_min_bytes: 10737418240", 100, 10 << 30},
	} {
		cfg, err := loadRCAYAML(t, "analyzer:\n  "+tc.yaml+"\n")
		if err != nil {
			t.Fatalf("%q: Load: %v", tc.yaml, err)
		}
		if got := cfg.Analyzer.IndexBloatThreshold(); got != tc.threshold {
			t.Errorf("%q: threshold = %d, want %d", tc.yaml, got, tc.threshold)
		}
		if got := cfg.Analyzer.IndexBloatFloorBytes(); got != tc.floor {
			t.Errorf("%q: floor = %d, want %d", tc.yaml, got, tc.floor)
		}
	}
}

func TestIndexBloat_NegativeFloorRefused(t *testing.T) {
	_, err := loadRCAYAML(t, "analyzer:\n  index_bloat_min_bytes: -1\n")
	if err == nil || !strings.Contains(err.Error(), "analyzer.index_bloat_min_bytes") {
		t.Fatalf("err = %v, want a refusal naming the key", err)
	}
}
