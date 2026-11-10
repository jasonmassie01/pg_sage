package config

import (
	"strings"
	"testing"
)

// forecast-04: forecaster.cache_min_blocks_per_day is the block accesses
// (blks_hit + blks_read over the day) below which a day's interval cache
// hit ratio is too thin to judge and the day is left out.

// No concurrent access tests: Load reads one file into a fresh Config.

func TestCacheMinBlocksPerDay_DefaultsWithoutConfig(t *testing.T) {
	cfg, _, err := loadWithWarnings(t, "mode: standalone\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Forecaster.CacheMinBlocksPerDay; got != 100_000 {
		t.Fatalf("cache_min_blocks_per_day = %d, want the default 100000", got)
	}
	if got := DefaultConfig().Forecaster.CacheMinBlocksPerDay; got != 100_000 {
		t.Fatalf("DefaultConfig cache_min_blocks_per_day = %d, want 100000", got)
	}
	// The threshold keeps its meaning: a fraction.
	if got := cfg.Forecaster.CacheWarnThreshold; got != 0.95 {
		t.Fatalf("cache_warn_threshold = %v, want the fraction 0.95", got)
	}
}

func TestCacheMinBlocksPerDay_LoadsAValue(t *testing.T) {
	cfg, _, err := loadWithWarnings(t,
		"forecaster:\n  cache_min_blocks_per_day: 5000\n  lookback_days: 14\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Forecaster.CacheMinBlocksPerDay != 5000 || cfg.Forecaster.LookbackDays != 14 {
		t.Fatalf("forecaster = %+v", cfg.Forecaster)
	}
}

func TestCacheMinBlocksPerDay_RejectsBelowOne(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		_, _, err := loadWithWarnings(t, "forecaster:\n  cache_min_blocks_per_day: "+v+"\n")
		if err == nil {
			t.Fatalf("cache_min_blocks_per_day %s loaded", v)
		}
		if !strings.Contains(err.Error(), "forecaster.cache_min_blocks_per_day") {
			t.Fatalf("value %s: error %q does not name the key", v, err)
		}
	}
	if _, _, err := loadWithWarnings(t,
		"forecaster:\n  cache_min_blocks_per_day: abc\n"); err == nil {
		t.Fatal("a non-numeric cache_min_blocks_per_day loaded")
	}
}

func TestCacheMinBlocksPerDay_Boundary(t *testing.T) {
	cfg, _, err := loadWithWarnings(t, "forecaster:\n  cache_min_blocks_per_day: 1\n")
	if err != nil || cfg.Forecaster.CacheMinBlocksPerDay != 1 {
		t.Fatalf("cache_min_blocks_per_day 1 = %+v, %v; want accepted", cfg, err)
	}
}
