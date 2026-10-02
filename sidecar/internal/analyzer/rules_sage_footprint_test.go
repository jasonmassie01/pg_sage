package analyzer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func footprint(total int64, tables ...int64) sageFootprint {
	fp := sageFootprint{database: "app", total: total}
	for i, b := range tables {
		fp.tables = append(fp.tables, sageTableSize{name: fmt.Sprintf("sage.t%d", i), bytes: b})
	}
	return fp
}

// Happy path: sage data above the configured share of the database is a
// warning naming the share, the limit and the largest sage tables.
func TestRuleSageFootprint_AboveLimitIsWarning(t *testing.T) {
	fp := footprint(2000, 900, 500, 300, 100, 80, 70, 50)
	got := ruleSageFootprint(fp, 10000, 10)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	f := got[0]
	if f.Category != "sage_footprint" || f.Severity != "warning" ||
		f.ObjectType != "database" || f.ObjectIdentifier != "app" {
		t.Fatalf("finding = %+v", f)
	}
	if f.Detail["sage_bytes"] != int64(2000) || f.Detail["database_bytes"] != int64(10000) ||
		f.Detail["share_pct"] != 20.0 || f.Detail["limit_pct"] != 10 {
		t.Fatalf("detail = %v", f.Detail)
	}
	largest, ok := f.Detail["largest_tables"].([]map[string]any)
	if !ok || len(largest) != 5 || largest[0]["table"] != "sage.t0" ||
		largest[0]["bytes"] != int64(900) || largest[4]["table"] != "sage.t4" {
		t.Fatalf("largest_tables = %v, want the top five in order", f.Detail["largest_tables"])
	}
	if !strings.Contains(f.Title, "20.0%") || !strings.Contains(f.Recommendation,
		"retention.sage_size_warning_pct") || f.RecommendedSQL != "" {
		t.Fatalf("title %q / recommendation %q / sql %q", f.Title, f.Recommendation,
			f.RecommendedSQL)
	}
}

// The finding is about pg_sage's own footprint but must not be dropped by
// the self-monitoring filter, which hides findings about sage objects.
func TestRuleSageFootprint_NotFilteredAsSelfMonitoring(t *testing.T) {
	got := ruleSageFootprint(footprint(5000, 5000), 10000, 10)
	if len(got) != 1 || isSelfMonitoringFinding(got[0]) {
		t.Fatalf("findings = %+v, want one that survives the self-monitoring filter", got)
	}
}

// Boundary: exactly at the limit is fine; one byte over is a finding.
func TestRuleSageFootprint_Boundary(t *testing.T) {
	if got := ruleSageFootprint(footprint(1000), 10000, 10); len(got) != 0 {
		t.Fatalf("at the limit: %d findings, want 0", len(got))
	}
	if got := ruleSageFootprint(footprint(1001), 10000, 10); len(got) != 1 {
		t.Fatalf("one byte over: %d findings, want 1", len(got))
	}
}

// Zero/invalid: a zero limit disables the guard; an unknown database size
// or an empty sage schema never produces a finding.
func TestRuleSageFootprint_DisabledOrUnknown(t *testing.T) {
	for name, tc := range map[string]struct {
		fp    sageFootprint
		db    int64
		limit int
	}{
		"disabled":         {footprint(9000), 10000, 0},
		"negative limit":   {footprint(9000), 10000, -1},
		"unknown db size":  {footprint(9000), 0, 10},
		"negative db size": {footprint(9000), -5, 10},
		"empty sage":       {footprint(0), 10000, 10},
	} {
		if got := ruleSageFootprint(tc.fp, tc.db, tc.limit); len(got) != 0 {
			t.Errorf("%s: %d findings, want 0", name, len(got))
		}
	}
}

func footprintAnalyzer(t *testing.T, limit int) *Analyzer {
	t.Helper()
	cfg := phase2Config()
	cfg.Retention.SageSizeWarningPct = limit
	a := New(phase2Pool(t), cfg, nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	return a
}

func snapshotWithDBSize(n int64) *collector.Snapshot {
	return &collector.Snapshot{System: collector.SystemStats{DBSizeBytes: n}}
}

// Integration: the real sage schema is measured; against a tiny database
// size it is over any limit, against a huge one under it. Both evaluate.
func TestCheckSageFootprint_RealSchema(t *testing.T) {
	a := footprintAnalyzer(t, 10)
	ctx := context.Background()
	got := a.checkSageFootprint(ctx, snapshotWithDBSize(1))
	if len(got) != 1 || got[0].Detail["sage_bytes"].(int64) <= 0 {
		t.Fatalf("tiny database: findings = %+v, want one with measured sage bytes", got)
	}
	if !a.eval.ok["sage_footprint"] || a.eval.failed["sage_footprint"] {
		t.Fatalf("eval = %+v, want sage_footprint evaluated", a.eval)
	}
	a.eval = newCycleEval()
	if got := a.checkSageFootprint(ctx, snapshotWithDBSize(1<<50)); len(got) != 0 {
		t.Fatalf("huge database: findings = %+v, want none", got)
	}
	if !a.eval.ok["sage_footprint"] {
		t.Fatal("huge database: not evaluated, an open finding would never resolve")
	}
}

// Error propagation: an unknown database size or a failing query fails the
// category (its open finding is kept, not resolved) and produces nothing.
func TestCheckSageFootprint_FailuresKeepOpenFinding(t *testing.T) {
	a := footprintAnalyzer(t, 10)
	if got := a.checkSageFootprint(context.Background(), snapshotWithDBSize(0)); got != nil ||
		!a.eval.failed["sage_footprint"] {
		t.Fatalf("unknown size: findings %v, failed=%v", got, a.eval.failed)
	}
	a.eval = newCycleEval()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := a.checkSageFootprint(cancelled, snapshotWithDBSize(1)); got != nil ||
		!a.eval.failed["sage_footprint"] {
		t.Fatalf("query error: findings %v, failed=%v", got, a.eval.failed)
	}
}

// State: a disabled guard issues no query and still evaluates, so a
// finding opened while it was enabled resolves.
func TestCheckSageFootprint_DisabledResolves(t *testing.T) {
	a := footprintAnalyzer(t, 0)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // any query would fail
	if got := a.checkSageFootprint(cancelled, snapshotWithDBSize(1)); got != nil ||
		!a.eval.ok["sage_footprint"] || a.eval.failed["sage_footprint"] {
		t.Fatalf("disabled: findings %v, eval %+v", got, a.eval)
	}
}
