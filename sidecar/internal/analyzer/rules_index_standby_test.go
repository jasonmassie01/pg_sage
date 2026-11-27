package analyzer

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// The exported standby gate is shared by every producer of an index drop:
// the unused_index rule and the tuning agent's covered_by drops (indexes-03).
// No concurrent access tests: both helpers are pure functions of their
// arguments and keep no state.

func gateFinding() Finding {
	return Finding{Category: "tuning_index_drop", Recommendation: "Drop it.",
		Detail: map[string]any{"covered_by": "orders_ab_idx"}}
}

func TestStandbyUsageUnknown_StreamingReplicaGates(t *testing.T) {
	snap := &collector.Snapshot{}
	replicaSnap(snap, 1)
	f := WithStandbyGate(gateFinding(), StandbyUsageUnknown(snap))
	reason, _ := f.Detail[DetailApprovalRequired].(string)
	if !strings.Contains(reason, "1 streaming replica(s)") {
		t.Fatalf("approval reason = %q, want the replica count", reason)
	}
	if f.Detail["replica_index_usage"] != "unknown" || f.Detail["streaming_replicas"] != 1 ||
		f.Detail["physical_slots"] != 0 {
		t.Fatalf("replica detail = %v", f.Detail)
	}
	if !strings.HasPrefix(f.Recommendation, "Drop it. Not dropped automatically: ") ||
		!strings.Contains(f.Recommendation, "pg_stat_user_indexes on each standby") {
		t.Fatalf("recommendation = %q", f.Recommendation)
	}
	if f.Detail["covered_by"] != "orders_ab_idx" {
		t.Fatalf("the gate keeps the producer's detail: %v", f.Detail)
	}
}

func TestStandbyUsageUnknown_NoReplicasLeavesTheFindingAlone(t *testing.T) {
	for name, snap := range map[string]*collector.Snapshot{
		"nil replication":   {},
		"empty replication": {Replication: &collector.ReplicationStats{}},
		"logical slot only": {Replication: &collector.ReplicationStats{
			Slots: []collector.SlotInfo{{SlotName: "cdc", SlotType: "logical"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := WithStandbyGate(gateFinding(), StandbyUsageUnknown(snap))
			for _, k := range []string{DetailApprovalRequired, "replica_index_usage",
				"streaming_replicas", "physical_slots"} {
				if _, has := f.Detail[k]; has {
					t.Fatalf("%s set without replicas: %v", k, f.Detail)
				}
			}
			if f.Recommendation != "Drop it." {
				t.Fatalf("recommendation = %q", f.Recommendation)
			}
		})
	}
}

func TestStandbyUsageUnknown_UnreadableStateIsUnknown(t *testing.T) {
	snap := &collector.Snapshot{Replication: &collector.ReplicationStats{},
		Unavailable: map[string]string{"replication": "permission denied"}}
	f := WithStandbyGate(gateFinding(), StandbyUsageUnknown(snap))
	reason, _ := f.Detail[DetailApprovalRequired].(string)
	if !strings.Contains(reason, "could not be read") ||
		f.Detail["replica_index_usage"] != "unknown" {
		t.Fatalf("unreadable replication must gate as unknown: %v", f.Detail)
	}
}

func TestStandbyUsageUnknown_NilSnapshotIsUnknown(t *testing.T) {
	f := WithStandbyGate(gateFinding(), StandbyUsageUnknown(nil))
	if f.Detail["replica_index_usage"] != "unknown" || f.Detail[DetailApprovalRequired] == nil {
		t.Fatalf("no snapshot is never \"no replicas\": %v", f.Detail)
	}
}

func TestWithStandbyGate_NilDetailIsCreated(t *testing.T) {
	snap := &collector.Snapshot{}
	replicaSnap(snap, 0, collector.SlotInfo{SlotName: "standby_b", SlotType: "physical"})
	f := WithStandbyGate(Finding{Recommendation: "Drop it."}, StandbyUsageUnknown(snap))
	if f.Detail["physical_slots"] != 1 || f.Detail["replica_index_usage"] != "unknown" {
		t.Fatalf("detail = %v", f.Detail)
	}
}
