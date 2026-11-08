package analyzer

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// slotDropApproval is why the drop of a slot is never run unattended.
const slotDropApproval = "irreversible; the consumer must resync"

// ruleInactiveSlots flags replication slots that stayed inactive past
// analyzer.inactive_slot_min_minutes (at once when wal_status shows WAL
// at risk), and active slots with high WAL retention (slow consumers
// like Debezium, Fivetran, or struggling replicas). Temporary slots, and
// synchronized failover slots on a standby (always inactive there, and
// not droppable), are not reported as inactive.
func ruleInactiveSlots(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	extras *RuleExtras,
) []Finding {
	if current.Replication == nil {
		// Unobserved: forget every clock so a gap never fires early.
		if extras != nil {
			extras.InactiveSlotSince = nil
			extras.InactiveSlotPending = false
		}
		return nil
	}
	now, clock := slotClock(extras)
	minIdle := inactiveSlotMinIdle(cfg)
	slowBytes := slowSlotThreshold(cfg)
	idle := map[string]bool{}
	pending := false
	var findings []Finding
	for _, s := range current.Replication.Slots {
		if s.Active {
			if s.RetainedBytes >= slowBytes {
				findings = append(findings, slowSlotFinding(s, slowBytes))
			}
			continue
		}
		if s.Temporary || (s.Synced && current.System.IsReplica) {
			continue
		}
		idle[s.SlotName] = true
		dur, known := slotIdleFor(s, now, clock)
		f, ok := inactiveSlotFinding(s, dur, known, minIdle)
		if ok {
			findings = append(findings, f)
		}
		pending = pending || (!ok && s.InactiveSince == nil)
	}
	for name := range clock {
		if !idle[name] {
			delete(clock, name)
		}
	}
	if extras != nil {
		extras.InactiveSlotPending = pending
	}
	return findings
}

// slotClock returns the rule clock and the first-inactive map (nil when
// there are no extras to keep it in).
func slotClock(extras *RuleExtras) (time.Time, map[string]time.Time) {
	if extras == nil {
		return time.Now(), nil
	}
	if extras.InactiveSlotSince == nil {
		extras.InactiveSlotSince = make(map[string]time.Time)
	}
	return extras.now(), extras.InactiveSlotSince
}

// slotIdleFor is how long the slot has been inactive: from the server's
// inactive_since (PG17+), else from the first cycle this process saw it
// inactive. known is false when neither source exists.
func slotIdleFor(
	s collector.SlotInfo, now time.Time, clock map[string]time.Time,
) (time.Duration, bool) {
	if s.InactiveSince != nil {
		return now.Sub(*s.InactiveSince), true
	}
	if clock == nil {
		return 0, false
	}
	first, seen := clock[s.SlotName]
	if !seen {
		clock[s.SlotName] = now
		return 0, true
	}
	return now.Sub(first), true
}

func inactiveSlotMinIdle(cfg *config.Config) time.Duration {
	minutes := config.DefaultInactiveSlotMinMinutes
	if cfg != nil && cfg.Analyzer.InactiveSlotMinMinutes > 0 {
		minutes = cfg.Analyzer.InactiveSlotMinMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// slowSlotThreshold is the active-slot WAL retention warning (1 GB
// default).
func slowSlotThreshold(cfg *config.Config) int64 {
	if cfg != nil && cfg.Analyzer.SlowSlotRetainedBytes != 0 {
		return cfg.Analyzer.SlowSlotRetainedBytes
	}
	return 1 << 30
}

// walStatusSeverity reports a slot at once whose WAL is at risk:
// unreserved (past max_slot_wal_keep_size, removed at the next
// checkpoint) and lost (already removed) are critical, extended (beyond
// max_wal_size) a warning. Other values and NULL wait for the clock.
func walStatusSeverity(status *string) string {
	if status == nil {
		return ""
	}
	switch *status {
	case "unreserved", "lost":
		return "critical"
	case "extended":
		return "warning"
	}
	return ""
}

func inactiveSlotFinding(
	s collector.SlotInfo, dur time.Duration, known bool, minIdle time.Duration,
) (Finding, bool) {
	severity := walStatusSeverity(s.WALStatus)
	if severity == "" {
		if !known || dur < minIdle {
			return Finding{}, false
		}
		severity = "warning"
	}
	detail := slotDetail(s)
	if known {
		detail["inactive_seconds"] = max(dur, 0).Seconds()
	}
	detail["manual_sql"] = fmt.Sprintf(
		"SELECT pg_drop_replication_slot(%s);", sanitize.QuoteLiteral(s.SlotName))
	detail[DetailApprovalRequired] = slotDropApproval
	return Finding{
		Category:         "inactive_slot",
		Severity:         severity,
		ObjectType:       "replication_slot",
		ObjectIdentifier: "slot:" + s.SlotName,
		Title: fmt.Sprintf("Inactive replication slot %s (retaining %s)",
			s.SlotName, humanBytes(s.RetainedBytes)),
		Detail:         detail,
		Recommendation: inactiveSlotAdvice(s.WALStatus),
		// Advisory only: dropping a slot cannot be undone and forces its
		// consumer to resync, so no SQL is ever run for this finding.
		ActionRisk: "high_risk",
	}, true
}

func inactiveSlotAdvice(status *string) string {
	const drop = " If the consumer is gone for good, drop the slot by hand " +
		"(detail.manual_sql) with approval: the drop is irreversible and " +
		"the consumer must resync from scratch."
	if status != nil {
		switch *status {
		case "lost":
			return "The slot has lost WAL its consumer still needed: the " +
				"consumer cannot continue from it and must resync." + drop
		case "unreserved":
			return "The slot is past max_slot_wal_keep_size: its WAL is " +
				"removed at the next checkpoint. Reconnect the consumer now." + drop
		case "extended":
			return "The slot retains more WAL than max_wal_size. Reconnect " +
				"its consumer (CDC connector, subscriber or standby)." + drop
		}
	}
	return "Find the slot's consumer (CDC connector, subscriber or standby) " +
		"and reconnect it." + drop
}

func slotDetail(s collector.SlotInfo) map[string]any {
	d := map[string]any{
		"slot_name":      s.SlotName,
		"slot_type":      s.SlotType,
		"retained_bytes": s.RetainedBytes,
		"active":         s.Active,
	}
	if s.WALStatus != nil {
		d["wal_status"] = *s.WALStatus
	}
	if s.SafeWALSize != nil {
		d["safe_wal_size"] = *s.SafeWALSize
	}
	if s.InactiveSince != nil {
		d["inactive_since"] = s.InactiveSince.UTC().Format(time.RFC3339)
	}
	if s.InvalidationReason != nil {
		d["invalidation_reason"] = *s.InvalidationReason
	}
	return d
}

func slowSlotFinding(s collector.SlotInfo, slowBytes int64) Finding {
	severity := "warning"
	if s.RetainedBytes >= slowBytes*5 {
		severity = "critical"
	}
	return Finding{
		Category:         "slow_replication_slot",
		Severity:         severity,
		ObjectType:       "replication_slot",
		ObjectIdentifier: "slot:" + s.SlotName,
		Title: fmt.Sprintf(
			"Active slot %s retaining %s -- slow consumer",
			s.SlotName, humanBytes(s.RetainedBytes),
		),
		Detail: map[string]any{
			"slot_name":      s.SlotName,
			"slot_type":      s.SlotType,
			"retained_bytes": s.RetainedBytes,
			"active":         s.Active,
		},
		Recommendation: "Investigate slow consumer " +
			"(Debezium, replica, CDC pipeline). " +
			"Slot holds WAL and xmin horizon, " +
			"blocking vacuum and risking disk exhaustion.",
		ActionRisk: "moderate",
	}
}
