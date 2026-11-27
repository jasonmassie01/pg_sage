package config

import (
	"math"
	"strings"
	"testing"
)

// vacuum-11b in the config-02b registry: an open api type excludes both
// bounds on every path (file, API, dashboard meta), and NaN is refused.
// No concurrent access tests: KeySpec checks are pure.

func TestKeySpecOpenRangeExcludesBothBounds(t *testing.T) {
	spec, ok := LookupKey("custodian.freeze.red_buffer_pct")
	if !ok || !spec.Open || !spec.HasRange || spec.Min != 0 || spec.Max != 100 ||
		spec.Type != "float_pct_open" {
		t.Fatalf("red_buffer_pct spec = %+v, want the open 0-100 float_pct_open range", spec)
	}
	for _, n := range []float64{0.001, 25, 99.999} {
		if err := spec.CheckOverride(n); err != nil {
			t.Errorf("CheckOverride(%g) = %v, want accepted", n, err)
		}
	}
	for _, n := range []float64{0, 100, -1, 100.5, math.NaN(), math.Inf(1)} {
		err := spec.CheckOverride(n)
		if err == nil || !strings.Contains(err.Error(),
			"custodian.freeze.red_buffer_pct must be greater than 0 and less than 100") {
			t.Errorf("CheckOverride(%g) = %v, want the open interval refusal", n, err)
		}
	}
}

func TestKeySpecClosedRangeKeepsBothBounds(t *testing.T) {
	spec, ok := LookupKey("custodian.wal.retained_wal_disk_pct_ceiling")
	if !ok || spec.Open {
		t.Fatalf("retained_wal_disk_pct_ceiling spec = %+v, want a closed range", spec)
	}
	if err := spec.CheckOverride(spec.Max); err != nil {
		t.Fatalf("CheckOverride(max %g) = %v, want accepted", spec.Max, err)
	}
	if err := spec.CheckOverride(math.NaN()); err == nil {
		t.Fatal("CheckOverride(NaN) accepted on a closed range")
	}
}
