package config

import (
	"strings"
	"testing"
)

// drill-01a: drill.max_concurrent_per_fleet defaults to 2 without a
// config file and is bounded 1-10. No concurrent access tests: pure
// functions of the config value.

func TestDrillConfig_DefaultsWithoutAFile(t *testing.T) {
	c := DefaultConfig()
	if c.Drill.MaxConcurrentPerFleet != 2 || c.Drill.MaxConcurrent() != 2 {
		t.Fatalf("default = %d, want 2", c.Drill.MaxConcurrentPerFleet)
	}
	if got := (DrillConfig{}).MaxConcurrent(); got != 2 {
		t.Fatalf("a zero section = %d, want the default 2", got)
	}
}

func TestDrillConfig_Bounds(t *testing.T) {
	for v, ok := range map[int]bool{0: false, -1: false, 1: true, 10: true, 11: false} {
		err := DrillConfig{MaxConcurrentPerFleet: v}.validate()
		if (err == nil) != ok {
			t.Errorf("max_concurrent_per_fleet %d: err %v, want ok=%v", v, err, ok)
		}
		if err != nil && !strings.Contains(err.Error(), "drill.max_concurrent_per_fleet") {
			t.Errorf("error %q does not name the key", err)
		}
	}
}
