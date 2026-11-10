package cases

import (
	"strings"
	"testing"
)

// migration-01 nil/empty and invalid-input cases for the measured
// preflight keys.

func measuredFinding(detail map[string]any) SourceFinding {
	detail["rule_id"] = "ddl_vacuum_full"
	return SourceFinding{Category: "migration_safety", RuleID: "ddl_vacuum_full",
		Detail: detail}
}

// A legacy finding (written before migration-01) has no measured keys:
// every live check renders unknown, never pass.
func TestPreflight_LegacyDetailStaysUnknown(t *testing.T) {
	report := ddlPreflightFromFinding(measuredFinding(map[string]any{}))
	for _, name := range []string{"activity", "pending_locks", "replica_lag",
		"table_size", "lock_timeout"} {
		if got := checkByName(report, name); got.Status != "unknown" {
			t.Errorf("legacy %s = %+v, want unknown", name, got)
		}
	}
	if report.MeasuredAt != "" {
		t.Fatalf("legacy MeasuredAt = %q", report.MeasuredAt)
	}
}

func TestPreflight_TableSizeNeedsKnownStats(t *testing.T) {
	tests := []struct {
		name   string
		detail map[string]any
		want   string
	}{
		{"zero size, stats unknown",
			map[string]any{"table_size_bytes": 0.0, "stats_known": false}, "unknown"},
		{"size read but never analysed",
			map[string]any{"table_size_bytes": 8192.0, "stats_known": false}, "unknown"},
		{"zero size, stats known",
			map[string]any{"table_size_bytes": 0.0, "stats_known": true}, "pass"},
		{"size, stats known",
			map[string]any{"table_size_bytes": 8192.0, "stats_known": true}, "pass"},
		{"just over 10 GiB",
			map[string]any{"table_size_bytes": float64(10<<30 + 1), "stats_known": true},
			"warn"},
		// Before migration-01 nothing wrote stats_known: a size alone keeps
		// its old rendering.
		{"size without stats_known", map[string]any{"table_size_bytes": 8192.0}, "pass"},
	}
	for _, tc := range tests {
		report := ddlPreflightFromFinding(measuredFinding(tc.detail))
		if got := checkByName(report, "table_size"); got.Status != tc.want {
			t.Errorf("%s: table_size = %+v, want %s", tc.name, got, tc.want)
		}
	}
}

func TestPreflight_MeasuredAtValidated(t *testing.T) {
	report := ddlPreflightFromFinding(measuredFinding(map[string]any{
		"measured_at": "2026-11-09T12:30:00Z", "active_queries": 0.0}))
	if report.MeasuredAt != "2026-11-09T12:30:00Z" {
		t.Fatalf("MeasuredAt = %q", report.MeasuredAt)
	}
	activity := checkByName(report, "activity")
	if activity.Status != "pass" || !strings.HasSuffix(activity.Detail,
		"(measured at detection)") {
		t.Fatalf("activity check = %+v", activity)
	}
	for _, bad := range []any{"yesterday", 1731155400.0, ""} {
		report = ddlPreflightFromFinding(measuredFinding(map[string]any{"measured_at": bad}))
		if report.MeasuredAt != "" {
			t.Errorf("measured_at %#v accepted as %q", bad, report.MeasuredAt)
		}
	}
}

// Invalid input: a negative or wrong-typed measurement is not a
// measurement.
func TestPreflight_InvalidMeasurementIsUnknown(t *testing.T) {
	report := ddlPreflightFromFinding(measuredFinding(map[string]any{
		"active_queries": -1.0, "pending_locks": "3"}))
	for _, name := range []string{"activity", "pending_locks"} {
		if got := checkByName(report, name); got.Status != "unknown" {
			t.Errorf("%s = %+v, want unknown", name, got)
		}
	}
}

// No concurrent access test: the preflight is a pure function of the
// finding.

// migration-01 review follow-up: estimated_lock_ms is the advisor's
// heuristic lock-hold estimate, not a measurement. Only a measured
// lock_timeout_ms is labelled measured.
func TestPreflight_LockEstimateIsLabelledEstimated(t *testing.T) {
	at := "2026-11-09T12:30:00Z"
	estimate := checkByName(ddlPreflightFromFinding(measuredFinding(map[string]any{
		"measured_at": at, "estimated_lock_ms": 50.0})), "lock_timeout")
	if estimate.Status != "pass" ||
		!strings.HasSuffix(estimate.Detail, "(estimated at detection)") ||
		strings.Contains(estimate.Detail, "measured") {
		t.Errorf("estimate-only lock_timeout check = %+v, want an estimate label", estimate)
	}
	measured := checkByName(ddlPreflightFromFinding(measuredFinding(map[string]any{
		"measured_at": at, "lock_timeout_ms": 2000.0, "estimated_lock_ms": 50.0})),
		"lock_timeout")
	if measured.Detail != "2000ms (measured at detection)" {
		t.Errorf("measured lock_timeout check = %+v", measured)
	}
	// Zero is no estimate: the check warns rather than passes.
	zero := checkByName(ddlPreflightFromFinding(measuredFinding(map[string]any{
		"measured_at": at, "estimated_lock_ms": 0.0})), "lock_timeout")
	if zero.Status != "warn" {
		t.Errorf("zero estimate lock_timeout check = %+v, want warn", zero)
	}
}
