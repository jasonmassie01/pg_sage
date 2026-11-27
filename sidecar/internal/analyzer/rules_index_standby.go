package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
)

// StandbyRisk says why this database's index usage on standbys is
// unknown. pg_stat_user_indexes counts only primary scans and pg_sage does
// not collect standby usage, so an index used only by read replicas looks
// unused here (G-P0-12). Every producer of an index drop applies it: the
// unused_index rule and the tuning agent's covered_by drops.
type StandbyRisk struct {
	reason    string // "" = no standbys: drops may be automatic
	streaming int    // pg_stat_replication rows
	physical  int    // physical replication slots
}

// StandbyUsageUnknown inspects the snapshot's replication state. Streaming
// replicas and physical slots (a standby that may be offline) both count;
// logical subscribers keep their own indexes. Unreadable replication state,
// or no snapshot at all, counts as unknown, never as "no replicas".
func StandbyUsageUnknown(snap *collector.Snapshot) StandbyRisk {
	if snap == nil || !snap.Available("replication") {
		return StandbyRisk{reason: "replication state could not be read this cycle, " +
			"so standby index usage is unknown"}
	}
	if snap.Replication == nil {
		return StandbyRisk{}
	}
	risk := StandbyRisk{streaming: len(snap.Replication.Replicas)}
	for _, slot := range snap.Replication.Slots {
		if slot.SlotType == "physical" {
			risk.physical++
		}
	}
	if risk.streaming == 0 && risk.physical == 0 {
		return StandbyRisk{}
	}
	risk.reason = fmt.Sprintf("this database has %d streaming replica(s) and %d "+
		"physical replication slot(s); pg_sage does not collect standby index usage, "+
		"so a read replica may still use this index", risk.streaming, risk.physical)
	return risk
}

// WithStandbyGate keeps the drop available to an operator but never
// automatic while standby usage is unknown, and says why in the finding.
func WithStandbyGate(f Finding, risk StandbyRisk) Finding {
	if risk.reason == "" {
		return f
	}
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	f.Detail[DetailApprovalRequired] = risk.reason
	f.Detail["replica_index_usage"] = "unknown"
	f.Detail["streaming_replicas"] = risk.streaming
	f.Detail["physical_slots"] = risk.physical
	f.Recommendation += " Not dropped automatically: " + risk.reason +
		". Check pg_stat_user_indexes on each standby before approving."
	return f
}
