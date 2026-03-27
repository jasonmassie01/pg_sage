package analyzer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func bloatySnapshot() *collector.Snapshot {
	return &collector.Snapshot{
		Tables: []collector.TableStats{
			{
				SchemaName: "public", RelName: "orders",
				NLiveTup: 5000, NDeadTup: 4000, // 44% dead
			},
		},
	}
}

func TestIOWaitRatio_NoQueries(t *testing.T) {
	snap := &collector.Snapshot{}
	if r := ioWaitRatio(snap); r != 0 {
		t.Errorf("expected 0, got %f", r)
	}
}

func TestIOWaitRatio_NoIOTime(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{TotalExecTime: 1000, BlkReadTime: 0, BlkWriteTime: 0},
		},
	}
	if r := ioWaitRatio(snap); r != 0 {
		t.Errorf("expected 0, got %f", r)
	}
}

func TestIOWaitRatio_HighIO(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{TotalExecTime: 1000, BlkReadTime: 400, BlkWriteTime: 300},
		},
	}
	r := ioWaitRatio(snap)
	if r < 0.69 || r > 0.71 {
		t.Errorf("expected ~0.70, got %f", r)
	}
}

func TestIOWaitRatio_CappedAtOne(t *testing.T) {
	// BlkReadTime can exceed TotalExecTime in edge cases (concurrent).
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{TotalExecTime: 100, BlkReadTime: 200, BlkWriteTime: 50},
		},
	}
	if r := ioWaitRatio(snap); r != 1.0 {
		t.Errorf("expected 1.0, got %f", r)
	}
}

func TestIOSaturated_Below(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{TotalExecTime: 1000, BlkReadTime: 100, BlkWriteTime: 100},
		},
	}
	if ioSaturated(snap) {
		t.Error("expected not saturated at 20% I/O")
	}
}

func TestIOSaturated_Above(t *testing.T) {
	snap := &collector.Snapshot{
		Queries: []collector.QueryStats{
			{TotalExecTime: 1000, BlkReadTime: 400, BlkWriteTime: 200},
		},
	}
	if !ioSaturated(snap) {
		t.Error("expected saturated at 60% I/O")
	}
}

func TestRuleTableBloat_NormalIO_Warning(t *testing.T) {
	cfg := testConfig()
	snap := bloatySnapshot()
	// Low I/O — should produce "warning".
	snap.Queries = []collector.QueryStats{
		{TotalExecTime: 1000, BlkReadTime: 50, BlkWriteTime: 50},
	}

	findings := ruleTableBloat(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "warning" {
		t.Errorf("expected warning, got %s", findings[0].Severity)
	}
	if findings[0].Detail["io_saturated"] != false {
		t.Error("expected io_saturated=false")
	}
}

func TestRuleTableBloat_HighIO_Downgraded(t *testing.T) {
	cfg := testConfig()
	snap := bloatySnapshot()
	// High I/O — should downgrade to "info".
	snap.Queries = []collector.QueryStats{
		{TotalExecTime: 1000, BlkReadTime: 400, BlkWriteTime: 200},
	}

	findings := ruleTableBloat(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "info" {
		t.Errorf("expected info (downgraded), got %s", findings[0].Severity)
	}
	if findings[0].Detail["io_saturated"] != true {
		t.Error("expected io_saturated=true")
	}
	rec := findings[0].Recommendation
	if rec == "Run VACUUM to reclaim dead tuple space." {
		t.Error("recommendation should mention I/O saturation")
	}
}

func TestRuleTableBloat_NoQueries_Warning(t *testing.T) {
	cfg := testConfig()
	snap := bloatySnapshot()
	// No queries means ioWaitRatio=0, not saturated.
	findings := ruleTableBloat(snap, nil, cfg, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != "warning" {
		t.Errorf("expected warning when no queries, got %s",
			findings[0].Severity)
	}
}
