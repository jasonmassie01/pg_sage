package analyzer

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests: claimFreshSnapshot and ruleHighPlanTime are
// only called from the single analyzer cycle goroutine.

func TestPreflightContractClaimFreshSnapshot(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	first := &collector.Snapshot{CollectedAt: t0}
	sameTimeCopy := &collector.Snapshot{CollectedAt: t0}
	older := &collector.Snapshot{CollectedAt: t0.Add(-time.Second)}
	newer := &collector.Snapshot{CollectedAt: t0.Add(time.Millisecond)}
	zeroA := &collector.Snapshot{}
	zeroB := &collector.Snapshot{}

	a := &Analyzer{logFn: func(string, string, ...any) {}}
	steps := []struct {
		name string
		snap *collector.Snapshot
		want bool
	}{
		{"first snapshot is fresh", first, true},
		{"same pointer is stale", first, false},
		{"same collected_at is stale", sameTimeCopy, false},
		{"older collected_at is stale", older, false},
		{"newer collected_at is fresh", newer, true},
		{"zero time new pointer is fresh", zeroA, true},
		{"zero time same pointer is stale", zeroA, false},
		{"zero time other pointer is fresh", zeroB, true},
	}
	for _, s := range steps {
		if got := a.claimFreshSnapshot(s.snap); got != s.want {
			t.Fatalf("%s: claimFreshSnapshot = %v, want %v", s.name, got, s.want)
		}
	}
	if a.lastAnalyzed != zeroB {
		t.Fatalf("lastAnalyzed = %p, want the last fresh snapshot %p", a.lastAnalyzed, zeroB)
	}
}

func TestPreflightContractStaleCycleLogsAndSkips(t *testing.T) {
	var logged []string
	a := &Analyzer{logFn: func(level, format string, _ ...any) {
		logged = append(logged, level+" "+format)
	}}
	snap := &collector.Snapshot{CollectedAt: time.Now()}
	if !a.claimFreshSnapshot(snap) {
		t.Fatal("first snapshot must be analyzed")
	}
	if len(logged) != 0 {
		t.Fatalf("fresh snapshot logged %v", logged)
	}
	if a.claimFreshSnapshot(snap) {
		t.Fatal("re-analysis of the same snapshot must be refused")
	}
	if len(logged) != 1 || logged[0][:4] != "WARN" {
		t.Fatalf("stale skip must log one WARN, got %v", logged)
	}
}

func TestPreflightContractZeroExecPlanRatioSkipped(t *testing.T) {
	cfg := config.DefaultConfig()
	cases := []struct {
		name      string
		execMs    float64
		wantCount int
		wantRatio float64
	}{
		{"zero exec mean has no ratio", 0, 0, 0},
		{"negative exec mean has no ratio", -1, 0, 0},
		{"tiny exec mean is still measured", 0.1, 1, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &collector.Snapshot{Queries: []collector.QueryStats{{
				QueryID: 42, Calls: 100, MeanPlanTime: 2, MeanExecTime: tc.execMs,
			}}}
			got := ruleHighPlanTime(snap, nil, cfg, nil)
			if len(got) != tc.wantCount {
				t.Fatalf("findings = %d, want %d", len(got), tc.wantCount)
			}
			if tc.wantCount == 0 {
				return
			}
			ratio, _ := got[0].Detail["ratio"].(float64)
			if math.IsInf(ratio, 0) || math.Abs(ratio-tc.wantRatio) > 1e-9 {
				t.Fatalf("ratio = %v, want %v", ratio, tc.wantRatio)
			}
			if got[0].Severity != "critical" {
				t.Fatalf("severity = %q, want critical for ratio 20", got[0].Severity)
			}
			if _, err := json.Marshal(got[0].Detail); err != nil {
				t.Fatalf("detail must be persistable: %v", err)
			}
		})
	}
}
