package analyzer

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// G-P0-12: unused-index drops use last_idx_scan (PG16+) and are never
// automatic while standby index usage is unknown.
//
// No concurrent access tests: ruleUnusedIndexes runs on the analyzer's
// single cycle goroutine; RuleExtras is not shared across goroutines.

func usageSnap(scans int64, last *time.Time) *collector.Snapshot {
	snap := unusedIndexSnap(scans)
	snap.Indexes[0].LastIdxScan = last
	return snap
}

func ago(d time.Duration) *time.Time {
	t := time.Now().Add(-d)
	return &t
}

func TestUnusedIndex_LastIdxScanOlderThanWindow(t *testing.T) {
	extras := defaultExtras()
	got := ruleUnusedIndexes(usageSnap(500, ago(30*24*time.Hour)), nil, unusedCfg(), extras)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1 for an index last scanned 30 days ago", len(got))
	}
	f := got[0]
	if f.RecommendedSQL == "" || f.RollbackSQL == "" {
		t.Errorf("stale-index finding lacks SQL: %+v", f)
	}
	if _, ok := f.Detail["last_idx_scan"].(string); !ok {
		t.Errorf("detail.last_idx_scan = %v, want the timestamp", f.Detail["last_idx_scan"])
	}
	if f.Detail["usage_evidence"] != "last_idx_scan" {
		t.Errorf("usage_evidence = %v, want last_idx_scan", f.Detail["usage_evidence"])
	}
	// No in-memory observation was needed: last_idx_scan is durable.
	if _, seen := extras.FirstSeen["public.idx_x"]; seen {
		t.Error("stale-by-last_idx_scan index should not need FirstSeen")
	}
}

func TestUnusedIndex_LastIdxScanRecentIsUsed(t *testing.T) {
	extras := defaultExtras()
	extras.FirstSeen["public.idx_x"] = time.Now().Add(-60 * 24 * time.Hour)
	got := ruleUnusedIndexes(usageSnap(500, ago(time.Hour)), nil, unusedCfg(), extras)
	if len(got) != 0 {
		t.Fatalf("recently scanned index flagged: %+v", got)
	}
	if _, seen := extras.FirstSeen["public.idx_x"]; seen {
		t.Error("a recent scan must restart the observation window")
	}
}

func TestUnusedIndex_LastIdxScanWindowBoundary(t *testing.T) {
	window := 7 * 24 * time.Hour
	if got := ruleUnusedIndexes(usageSnap(5, ago(window+time.Minute)), nil, unusedCfg(),
		defaultExtras()); len(got) != 1 {
		t.Errorf("just past the window: findings = %d, want 1", len(got))
	}
	if got := ruleUnusedIndexes(usageSnap(5, ago(window-time.Minute)), nil, unusedCfg(),
		defaultExtras()); len(got) != 0 {
		t.Errorf("just inside the window: findings = %d, want 0", len(got))
	}
}

// Pre-PG16 there is no last_idx_scan: any scan since the stats epoch
// still counts as use (unchanged behavior).
func TestUnusedIndex_NoLastIdxScanKeepsCounterRule(t *testing.T) {
	extras := defaultExtras()
	if got := ruleUnusedIndexes(usageSnap(5, nil), nil, unusedCfg(), extras); len(got) != 0 {
		t.Fatalf("scanned index without last_idx_scan flagged: %+v", got)
	}
}

func replicaSnap(snap *collector.Snapshot, replicas int, slots ...collector.SlotInfo) {
	rs := &collector.ReplicationStats{Slots: slots}
	for i := 0; i < replicas; i++ {
		rs.Replicas = append(rs.Replicas, collector.ReplicaInfo{State: "streaming"})
	}
	snap.Replication = rs
}

func staleFinding(t *testing.T, mutate func(*collector.Snapshot)) Finding {
	t.Helper()
	snap := usageSnap(0, nil)
	mutate(snap)
	extras := defaultExtras()
	extras.FirstSeen["public.idx_x"] = time.Now().Add(-30 * 24 * time.Hour)
	got := ruleUnusedIndexes(snap, nil, unusedCfg(), extras)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	return got[0]
}

func TestUnusedIndex_ReplicasRequireApproval(t *testing.T) {
	f := staleFinding(t, func(s *collector.Snapshot) { replicaSnap(s, 2) })
	reason, _ := f.Detail[DetailApprovalRequired].(string)
	if !strings.Contains(reason, "replica") {
		t.Fatalf("approval reason = %q, want it to explain the replicas", reason)
	}
	if f.Detail["replica_index_usage"] != "unknown" || f.Detail["streaming_replicas"] != 2 {
		t.Errorf("replica detail = %v / %v", f.Detail["replica_index_usage"],
			f.Detail["streaming_replicas"])
	}
	if f.RecommendedSQL == "" {
		t.Error("an operator must still be able to approve the drop")
	}
	if !strings.Contains(f.Recommendation, "replica") {
		t.Errorf("recommendation %q does not explain the refusal", f.Recommendation)
	}
}

func TestUnusedIndex_PhysicalSlotCountsAsStandby(t *testing.T) {
	f := staleFinding(t, func(s *collector.Snapshot) {
		replicaSnap(s, 0, collector.SlotInfo{SlotName: "standby_b", SlotType: "physical"})
	})
	if reason, _ := f.Detail[DetailApprovalRequired].(string); reason == "" {
		t.Fatal("a physical slot (a standby that may be offline) must require approval")
	}
}

func TestUnusedIndex_LogicalSlotOnlyStaysAutonomous(t *testing.T) {
	f := staleFinding(t, func(s *collector.Snapshot) {
		replicaSnap(s, 0, collector.SlotInfo{SlotName: "cdc", SlotType: "logical"})
	})
	if _, marked := f.Detail[DetailApprovalRequired]; marked {
		t.Errorf("logical subscribers do not read this index: %v", f.Detail)
	}
}

func TestUnusedIndex_ReplicationUnknownRequiresApproval(t *testing.T) {
	f := staleFinding(t, func(s *collector.Snapshot) {
		s.Unavailable = map[string]string{"replication": "permission denied"}
	})
	reason, _ := f.Detail[DetailApprovalRequired].(string)
	if !strings.Contains(reason, "could not be read") {
		t.Fatalf("approval reason = %q, want unknown-replication wording", reason)
	}
}

func TestUnusedIndex_NoReplicasStaysAutonomous(t *testing.T) {
	for name, mutate := range map[string]func(*collector.Snapshot){
		"nil replication":   func(*collector.Snapshot) {},
		"empty replication": func(s *collector.Snapshot) { replicaSnap(s, 0) },
	} {
		f := staleFinding(t, mutate)
		if _, marked := f.Detail[DetailApprovalRequired]; marked {
			t.Errorf("%s: drop marked for approval without replicas", name)
		}
		if f.ActionRisk != "safe" {
			t.Errorf("%s: ActionRisk = %q, want safe", name, f.ActionRisk)
		}
	}
}
