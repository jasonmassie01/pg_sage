package analyzer

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// cdcStallClock is when an active logical slot was first seen at its
// current confirmed_flush_lsn, and its byte lag then. moved is set when the
// clock started on an observed move rather than on a first sighting (after
// a restart, or when the slot became active).
type cdcStallClock struct {
	lsn   string
	since time.Time
	lag   int64
	moved bool
}

func cdcStallLimit(cfg *config.Config) time.Duration {
	m := config.DefaultCDCStallMinutes
	if cfg != nil && cfg.Analyzer.CDCStallMinutes > 0 {
		m = cfg.Analyzer.CDCStallMinutes
	}
	return time.Duration(m) * time.Minute
}

// stallTracked reports whether the stall clock times s: an active,
// permanent logical slot with a confirmed position (inactive slots are
// inactive_slot's; synced slots on a standby are never consumed there).
func stallTracked(s collector.SlotInfo, standby bool) bool {
	return s.SlotType == "logical" && s.Active && !s.Temporary &&
		(!s.Synced || !standby) && s.ConfirmedFlushLSN != nil &&
		s.ConfirmedFlushLagBytes != nil
}

// ruleCDCConsumerStalled flags an active logical slot whose
// confirmed_flush_lsn stood still for cdc_stall_minutes while WAL advanced
// (its lag grew): a CDC connector that holds the slot but no longer
// confirms, as a Debezium connector can. The clock is kept across a cycle
// whose slot list was not read: a position the same after the gap never
// moved during it.
func ruleCDCConsumerStalled(
	cur, _ *collector.Snapshot, cfg *config.Config, extras *RuleExtras,
) []Finding {
	if !cur.Available("replication") {
		return nil
	}
	now, clocks := time.Now(), map[string]cdcStallClock{}
	if extras != nil {
		now = extras.now()
		if extras.cdcStall != nil {
			clocks = extras.cdcStall
		}
	}
	limit := cdcStallLimit(cfg)
	next := map[string]cdcStallClock{}
	var out []Finding
	var slots []collector.SlotInfo
	if cur.Replication != nil {
		slots = cur.Replication.Slots
	}
	for _, s := range slots {
		if !stallTracked(s, cur.System.IsReplica) {
			continue
		}
		lsn, lag := *s.ConfirmedFlushLSN, *s.ConfirmedFlushLagBytes
		c, seen := clocks[s.SlotName]
		if !seen || c.lsn != lsn {
			next[s.SlotName] = cdcStallClock{lsn: lsn, since: now, lag: lag, moved: seen}
			if !seen {
				extras.holdOpen("cdc_consumer_stalled", "slot:"+s.SlotName)
			}
			continue
		}
		next[s.SlotName] = c
		stalled := now.Sub(c.since)
		switch {
		case stalled >= limit && lag > c.lag:
			out = append(out, cdcStalledFinding(s, stalled))
		case !c.moved && stalled < limit:
			extras.holdOpen("cdc_consumer_stalled", "slot:"+s.SlotName)
		}
	}
	if extras != nil {
		extras.cdcStall = next
	}
	return out
}

func cdcStalledFinding(s collector.SlotInfo, stalled time.Duration) Finding {
	return Finding{
		Category:         "cdc_consumer_stalled",
		Severity:         "warning",
		ObjectType:       "replication_slot",
		ObjectIdentifier: "slot:" + s.SlotName,
		Title: fmt.Sprintf("CDC slot %s is active but its position has not moved for %s",
			s.SlotName, stalled.Round(time.Minute)),
		Detail: map[string]any{
			"slot_name":                 s.SlotName,
			"confirmed_flush_lsn":       *s.ConfirmedFlushLSN,
			"confirmed_flush_lag_bytes": *s.ConfirmedFlushLagBytes,
			"retained_bytes":            s.RetainedBytes,
			"stalled_seconds":           stalled.Seconds(),
		},
		Recommendation: "WAL kept advancing but the consumer holding this slot " +
			"(Debezium, another CDC connector or a subscriber) confirmed nothing, so " +
			"the slot retains WAL and holds the xmin horizon. Check the connector's " +
			"health and offsets; a connector that only reads tables with little " +
			"traffic needs a heartbeat (Debezium heartbeat.interval.ms) to advance.",
		ActionRisk: "moderate",
	}
}

// ruleCDCHeartbeatStale flags a configured CDC heartbeat whose newest value
// is at least its max_age_minutes old, or whose table has no row. A
// heartbeat that could not be read is unknown: its open finding is kept.
func ruleCDCHeartbeatStale(
	cur, _ *collector.Snapshot, _ *config.Config, extras *RuleExtras,
) []Finding {
	var out []Finding
	for _, r := range cur.CDCHeartbeats {
		ident := "cdc_heartbeat:" + r.Slot
		if r.Err != "" {
			extras.holdOpen("cdc_heartbeat_stale", ident)
			continue
		}
		d := map[string]any{
			"slot_name": r.Slot, "table": r.Table, "column": r.Column,
			"max_age_seconds": r.MaxAge.Seconds(),
		}
		title := fmt.Sprintf("CDC heartbeat %s has no row", r.Table)
		if r.Latest == nil {
			d["no_rows"] = true
		} else {
			age := cur.CollectedAt.Sub(*r.Latest)
			if age < r.MaxAge {
				continue
			}
			d["age_seconds"] = age.Seconds()
			d["latest"] = r.Latest.UTC().Format(time.RFC3339)
			title = fmt.Sprintf("CDC heartbeat %s last written %s ago (slot %s)",
				r.Table, age.Round(time.Second), r.Slot)
		}
		out = append(out, Finding{
			Category: "cdc_heartbeat_stale", Severity: "warning",
			ObjectType: "replication_slot", ObjectIdentifier: ident,
			Title: title, Detail: d, ActionRisk: "moderate",
			Recommendation: "The CDC connector on this slot stopped writing its " +
				"heartbeat: it is down, stuck or disconnected. Check the connector " +
				"and its heartbeat setting before the slot's WAL retention grows.",
		})
	}
	return out
}
