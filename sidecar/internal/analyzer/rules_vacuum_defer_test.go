package analyzer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// vacuum-06: a VACUUM deferred for I/O pressure carries no SQL, so neither
// the recommendation store nor the executor can act on it while the system
// is I/O-bound.

// No concurrent access tests: ruleTableBloat is a pure function of its
// snapshot and config and shares no state.

func snapshotWithIO(read, write float64) *collector.Snapshot {
	snap := bloatySnapshot()
	snap.Queries = []collector.QueryStats{
		{TotalExecTime: 1000, BlkReadTime: read, BlkWriteTime: write},
	}
	return snap
}

func TestRuleTableBloat_SaturatedDefersWithoutSQL(t *testing.T) {
	findings := ruleTableBloat(snapshotWithIO(400, 200), nil, testConfig(), nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.RecommendedSQL != "" {
		t.Errorf("RecommendedSQL = %q, want empty while I/O is saturated", f.RecommendedSQL)
	}
	if f.ActionRisk != "" {
		t.Errorf("ActionRisk = %q, want empty for a deferred finding", f.ActionRisk)
	}
	if f.Severity != "info" {
		t.Errorf("Severity = %q, want info", f.Severity)
	}
	if got := f.Detail["deferred_reason"]; got != "io_saturated" {
		t.Errorf("deferred_reason = %v, want io_saturated", got)
	}
	if f.Detail["io_saturated"] != true {
		t.Errorf("io_saturated = %v, want true", f.Detail["io_saturated"])
	}
	if !strings.Contains(f.Recommendation, "deferring") {
		t.Errorf("Recommendation = %q, want it to say the VACUUM is deferred",
			f.Recommendation)
	}
	if f.Category != "table_bloat" || f.ObjectIdentifier != "public.orders" {
		t.Errorf("finding = %s %s, want table_bloat public.orders",
			f.Category, f.ObjectIdentifier)
	}
}

func TestRuleTableBloat_UnsaturatedKeepsVacuumSQL(t *testing.T) {
	findings := ruleTableBloat(snapshotWithIO(50, 50), nil, testConfig(), nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.RecommendedSQL != `VACUUM "public"."orders";` {
		t.Errorf("RecommendedSQL = %q, want the quoted VACUUM", f.RecommendedSQL)
	}
	if f.ActionRisk != "safe" {
		t.Errorf("ActionRisk = %q, want safe", f.ActionRisk)
	}
	if _, ok := f.Detail["deferred_reason"]; ok {
		t.Errorf("deferred_reason = %v, want no key when not deferred",
			f.Detail["deferred_reason"])
	}
}

// Boundary: saturation is strictly above 50% of execution time in I/O.
func TestRuleTableBloat_SaturationBoundary(t *testing.T) {
	cases := []struct {
		name        string
		read, write float64
		wantSQL     bool
	}{
		{"exactly 50% keeps SQL", 300, 200, true},
		{"just above 50% defers", 300, 201, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := ruleTableBloat(snapshotWithIO(tc.read, tc.write), nil,
				testConfig(), nil)
			if len(findings) != 1 {
				t.Fatalf("expected 1 finding, got %d", len(findings))
			}
			if got := findings[0].RecommendedSQL != ""; got != tc.wantSQL {
				t.Errorf("has SQL = %v, want %v (SQL %q)", got, tc.wantSQL,
					findings[0].RecommendedSQL)
			}
		})
	}
}

// Unlogged tables keep their note when the VACUUM is deferred.
func TestRuleTableBloat_SaturatedUnloggedKeepsNote(t *testing.T) {
	snap := snapshotWithIO(400, 200)
	snap.Tables[0].Relpersistence = "u"
	findings := ruleTableBloat(snap, nil, testConfig(), nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	f := findings[0]
	if f.RecommendedSQL != "" || !strings.Contains(f.Recommendation, "UNLOGGED") ||
		f.Detail["deferred_reason"] != "io_saturated" {
		t.Errorf("finding = %+v, want a deferred finding that names UNLOGGED", f)
	}
}

// Empty input: no tables, no findings, whatever the I/O.
func TestRuleTableBloat_SaturatedNoTables(t *testing.T) {
	snap := snapshotWithIO(400, 200)
	snap.Tables = nil
	if findings := ruleTableBloat(snap, nil, testConfig(), nil); len(findings) != 0 {
		t.Errorf("findings = %+v, want none", findings)
	}
}
