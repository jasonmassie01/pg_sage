package store

import (
	"strings"
	"testing"
)

// vacuum-11b: an API or dashboard override of custodian.freeze.red_buffer_pct
// is held to the same open interval as the config file, 0 < value < 100,
// so no stored value is silently replaced by the custodian's 25 fallback.
func TestRedBufferPctOverrideOpenInterval(t *testing.T) {
	const key = "custodian.freeze.red_buffer_pct"
	for _, value := range []string{"25", "99.9", "99.999", "0.0001", "0.5"} {
		if err := ValidateConfigOverride(key, value); err != nil {
			t.Errorf("ValidateConfigOverride(%s) = %v, want accepted", value, err)
		}
	}
	for _, value := range []string{"100", "150", "0", "-1", "NaN", "Inf", "abc"} {
		err := ValidateConfigOverride(key, value)
		if err == nil {
			t.Errorf("ValidateConfigOverride(%s) accepted", value)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("ValidateConfigOverride(%s) = %q, want it to name the key", value, err)
		}
	}
	err := ValidateConfigOverride(key, "100")
	if err == nil || !strings.Contains(err.Error(), "greater than 0 and less than 100") {
		t.Fatalf("ValidateConfigOverride(100) = %v, want the open interval stated", err)
	}
}

// A row stored before the interval tightened is skipped on replay, with
// a warning, so the file value applies; it never fails a restart.
func TestRedBufferPctStoredHundredIsSkippedOnReplay(t *testing.T) {
	apply, ok, problem := ReplayOverride("custodian.freeze.red_buffer_pct", "100")
	if ok || apply != "" {
		t.Fatalf("ReplayOverride(100) = (%q, %v), want skipped", apply, ok)
	}
	if !strings.Contains(problem, "float_pct_open") {
		t.Fatalf("problem = %q, want it to name the failed check", problem)
	}
	apply, ok, _ = ReplayOverride("custodian.freeze.red_buffer_pct", "40")
	if !ok || apply != "40" {
		t.Fatalf("ReplayOverride(40) = (%q, %v), want applied", apply, ok)
	}
}

// The WAL ceiling shares no rule with the freeze buffer: 100 still loads
// in the config file there, so the API keeps accepting it.
func TestRetainedWALCeilingStillAcceptsHundred(t *testing.T) {
	key := "custodian.wal.retained_wal_disk_pct_ceiling"
	if err := ValidateConfigOverride(key, "100"); err != nil {
		t.Fatalf("ValidateConfigOverride(%s, 100) = %v, want accepted", key, err)
	}
}

// NaN compares false with both bounds; every float override type refuses it.
func TestFloatOverridesRefuseNaN(t *testing.T) {
	for _, key := range []string{
		"analyzer.wraparound_freeze_share",
		"custodian.wal.retained_wal_disk_pct_ceiling",
		"custodian.freeze.red_buffer_pct",
	} {
		if err := ValidateConfigOverride(key, "NaN"); err == nil {
			t.Errorf("ValidateConfigOverride(%s, NaN) accepted", key)
		}
	}
}

// No concurrent access tests: validation is pure and shares no state.
