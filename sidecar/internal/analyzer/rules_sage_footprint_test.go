package analyzer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// floor is the sage schema size below which the footprint is never a
// finding: the snapshot cap's minimum. The rule tests work in units of it
// so that a share over the limit is not hidden by the floor.
const floor = config.MinSnapshotCapBytes

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
	u := floor / 1000
	fp := footprint(2000*u, 900*u, 500*u, 300*u, 100*u, 80*u, 70*u, 50*u)
	got := ruleSageFootprint(fp, 10000*u, 10)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	f := got[0]
	if f.Category != "sage_footprint" || f.Severity != "warning" ||
		f.ObjectType != "database" || f.ObjectIdentifier != "app" {
		t.Fatalf("finding = %+v", f)
	}
	if f.Detail["sage_bytes"] != 2000*u || f.Detail["database_bytes"] != 10000*u ||
		f.Detail["share_pct"] != 20.0 || f.Detail["limit_pct"] != 10 {
		t.Fatalf("detail = %v", f.Detail)
	}
	largest, ok := f.Detail["largest_tables"].([]map[string]any)
	if !ok || len(largest) != 5 || largest[0]["table"] != "sage.t0" ||
		largest[0]["bytes"] != 900*u || largest[4]["table"] != "sage.t4" {
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
	got := ruleSageFootprint(footprint(5*floor, 5*floor), 10*floor, 10)
	if len(got) != 1 || isSelfMonitoringFinding(got[0]) {
		t.Fatalf("findings = %+v, want one that survives the self-monitoring filter", got)
	}
}

// Boundary: exactly at the limit is fine; one byte over is a finding.
func TestRuleSageFootprint_Boundary(t *testing.T) {
	if got := ruleSageFootprint(footprint(2*floor), 20*floor, 10); len(got) != 0 {
		t.Fatalf("at the limit: %d findings, want 0", len(got))
	}
	if got := ruleSageFootprint(footprint(2*floor+1), 20*floor, 10); len(got) != 1 {
		t.Fatalf("one byte over: %d findings, want 1", len(got))
	}
}

// Boundary: on a small database the sage schema is a large share of it
// (a fresh install measured 20.1%), but below the snapshot cap's floor it
// is never a finding. At the floor and above it the share decides.
func TestRuleSageFootprint_AbsoluteFloor(t *testing.T) {
	small := int64(100 << 20) // a database far smaller than the floor
	if got := ruleSageFootprint(footprint(floor-1), small, 10); len(got) != 0 {
		t.Fatalf("just under the floor: %d findings, want 0", len(got))
	}
	if got := ruleSageFootprint(footprint(20<<20), small, 10); len(got) != 0 {
		t.Fatalf("a 20%% share of a tiny database: %d findings, want 0", len(got))
	}
	for name, total := range map[string]int64{"at": floor, "above": floor + 1} {
		got := ruleSageFootprint(footprint(total), small, 10)
		if len(got) != 1 || got[0].Detail["sage_bytes"] != total {
			t.Fatalf("%s the floor: findings = %+v, want one", name, got)
		}
	}
}

// Happy path on a large database: 12 GB of sage data in 100 GB is still a
// finding; 5 GB is not.
func TestRuleSageFootprint_LargeDatabaseStillWarns(t *testing.T) {
	db := int64(100 << 30)
	got := ruleSageFootprint(footprint(12<<30), db, 10)
	if len(got) != 1 || got[0].Detail["share_pct"] != 12.0 ||
		!strings.Contains(got[0].Title, "12.0%") {
		t.Fatalf("12 GB of 100 GB: findings = %+v, want one at 12.0%%", got)
	}
	if got := ruleSageFootprint(footprint(5<<30), db, 10); len(got) != 0 {
		t.Fatalf("5 GB of 100 GB: %d findings, want 0", len(got))
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
		"disabled":         {footprint(9 * floor), 10 * floor, 0},
		"negative limit":   {footprint(9 * floor), 10 * floor, -1},
		"unknown db size":  {footprint(9 * floor), 0, 10},
		"negative db size": {footprint(9 * floor), -5, 10},
		"empty sage":       {footprint(0), 10 * floor, 10},
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

// Integration: the real sage schema of a test database is measured and is
// below the floor, so even against a tiny database size it is no finding
// (the fresh-install noise); against a huge one it is under the limit.
// Both evaluate, so an open finding resolves.
func TestCheckSageFootprint_RealSchema(t *testing.T) {
	a := footprintAnalyzer(t, 10)
	ctx := context.Background()
	fp, err := a.measureSageFootprint(ctx)
	if err != nil || fp.total <= 0 || fp.total >= floor {
		t.Fatalf("measured sage schema = %d bytes (err %v), want 0 < size < floor", fp.total,
			err)
	}
	if got := a.checkSageFootprint(ctx, snapshotWithDBSize(1)); len(got) != 0 {
		t.Fatalf("tiny database: findings = %+v, want none below the floor", got)
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
