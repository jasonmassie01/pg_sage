package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// market-13: CDC consumer health. cdc_consumer_stalled fires when WAL
// advanced but an active logical slot's confirmed_flush_lsn did not move for
// analyzer.cdc_stall_minutes (a Debezium slot can stay active while its
// position stands still). cdc_heartbeat_stale fires when a configured
// heartbeat table's newest value is older than its max_age_minutes. The
// stall clock lives in memory in the analyzer's RuleExtras.
//
// No concurrent-access tests: the rules run on the analyzer's goroutine
// and the stall clock is only touched there.

// cdcSlot is an active logical slot at confirmed_flush_lsn lsn, lag bytes
// behind the server's WAL position.
func cdcSlot(name, lsn string, lag int64) collector.SlotInfo {
	return collector.SlotInfo{
		SlotName: name, SlotType: "logical", Active: true,
		RetainedBytes: lag, ConfirmedFlushLSN: strPtr(lsn), ConfirmedFlushLagBytes: i64(lag),
		InCurrentDatabase: true,
	}
}

func stallFindings(snap *collector.Snapshot, ex *RuleExtras) []Finding {
	return ruleCDCConsumerStalled(snap, nil, phase2Config(), ex)
}

// State transitions at the default 10 minutes: 9 minutes stalled gives no
// finding, 11 minutes gives cdc_consumer_stalled, and a move clears it.
func TestCDCStall_ClockFiresAfterThresholdAndClearsOnMove(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	steps := []struct {
		after time.Duration
		lsn   string
		lag   int64
		want  int
	}{
		{0, "0/1000", 1000, 0},
		{9 * time.Minute, "0/1000", 50000, 0},
		{11 * time.Minute, "0/1000", 90000, 1},
		{12 * time.Minute, "0/9000", 4000, 0},
		{21 * time.Minute, "0/9000", 4000, 0},
	}
	for _, st := range steps {
		clk.now = slotT0.Add(st.after)
		got := stallFindings(slotsSnap(cdcSlot("debezium", st.lsn, st.lag)), ex)
		if len(got) != st.want {
			t.Fatalf("at +%s: %d findings %+v, want %d", st.after, len(got), got, st.want)
		}
		if st.want == 0 {
			continue
		}
		f := got[0]
		if f.Category != "cdc_consumer_stalled" || f.ObjectIdentifier != "slot:debezium" ||
			f.Severity != "warning" || f.RecommendedSQL != "" {
			t.Fatalf("finding = %+v, want an advisory cdc_consumer_stalled on slot:debezium", f)
		}
		if f.Detail["confirmed_flush_lsn"] != "0/1000" ||
			f.Detail["confirmed_flush_lag_bytes"] != int64(90000) ||
			f.Detail["stalled_seconds"] != (11*time.Minute).Seconds() {
			t.Errorf("detail = %v, want lsn, lag and stalled_seconds", f.Detail)
		}
	}
}

// An idle server whose WAL did not move is not a stall: the consumer has
// nothing to confirm.
func TestCDCStall_NoWALAdvanceNoFinding(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	for _, after := range []time.Duration{0, 30 * time.Minute, 3 * time.Hour} {
		clk.now = slotT0.Add(after)
		if got := stallFindings(slotsSnap(cdcSlot("idle", "0/1000", 1000)), ex); len(got) != 0 {
			t.Fatalf("at +%s with no WAL advance: %+v, want none", after, got)
		}
	}
}

// Boundary: the threshold follows analyzer.cdc_stall_minutes.
func TestCDCStall_ConfiguredThreshold(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	cfg := phase2Config()
	cfg.Analyzer.CDCStallMinutes = 30
	for _, st := range []struct {
		after time.Duration
		want  int
	}{{0, 0}, {29 * time.Minute, 0}, {30 * time.Minute, 1}} {
		clk.now = slotT0.Add(st.after)
		snap := slotsSnap(cdcSlot("s", "0/1000", 1000+int64(st.after.Seconds())))
		if got := ruleCDCConsumerStalled(snap, nil, cfg, ex); len(got) != st.want {
			t.Fatalf("at +%s: %d findings, want %d", st.after, len(got), st.want)
		}
	}
}

// Inactive, physical and temporary slots are not CDC consumers this rule
// times: inactive_slot owns an inactive slot.
func TestCDCStall_SkipsInactivePhysicalAndTemporarySlots(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	idle := cdcSlot("idle", "0/1000", 1000)
	idle.Active = false
	phys := cdcSlot("standby", "0/1000", 1000)
	phys.SlotType, phys.ConfirmedFlushLSN, phys.ConfirmedFlushLagBytes = "physical", nil, nil
	tmp := cdcSlot("tmp", "0/1000", 1000)
	tmp.Temporary = true
	for _, after := range []time.Duration{0, time.Hour} {
		clk.now = slotT0.Add(after)
		idle.ConfirmedFlushLagBytes = i64(1000 + int64(after))
		tmp.ConfirmedFlushLagBytes = i64(1000 + int64(after))
		if got := stallFindings(slotsSnap(idle, phys, tmp), ex); len(got) != 0 {
			t.Fatalf("at +%s: %+v, want none", after, got)
		}
	}
}

// An unread replication category keeps the clock: an LSN that is the same
// after the gap never moved during it.
func TestCDCStall_UnreadCycleKeepsClock(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	stallFindings(slotsSnap(cdcSlot("s", "0/1000", 1000)), ex)
	clk.now = slotT0.Add(5 * time.Minute)
	gap := &collector.Snapshot{Unavailable: map[string]string{"replication": "timeout"}}
	if got := stallFindings(gap, ex); got != nil {
		t.Fatalf("unread cycle: %+v, want nil", got)
	}
	clk.now = slotT0.Add(11 * time.Minute)
	if got := stallFindings(slotsSnap(cdcSlot("s", "0/1000", 9000)), ex); len(got) != 1 {
		t.Fatalf("after the gap: %d findings, want 1", len(got))
	}
}

// Nil/empty: no slots at all clears the clocks.
func TestCDCStall_NoSlotsClearsClock(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	stallFindings(slotsSnap(cdcSlot("s", "0/1000", 1000)), ex)
	clk.now = slotT0.Add(time.Minute)
	if got := stallFindings(&collector.Snapshot{}, ex); len(got) != 0 {
		t.Fatalf("no slots: %+v", got)
	}
	clk.now = slotT0.Add(15 * time.Minute)
	if got := stallFindings(slotsSnap(cdcSlot("s", "0/1000", 9000)), ex); len(got) != 0 {
		t.Fatalf("slot back after a cycle without it: %+v, want a fresh clock", got)
	}
}

// After a restart the clock starts on first sight: below the threshold the
// open finding is kept, not resolved; a slot whose LSN moved resolves.
func TestCDCStall_FirstSightHoldsMoveResolves(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	a := slotHoldAnalyzer(clk)
	snap := slotsSnap(cdcSlot("s", "0/1000", 1000))
	ok, kept, n := subCycle(a, snap, nil, "cdc_consumer_stalled")
	if !ok || n != 0 || len(kept) != 1 || kept[0] != "slot:s" {
		t.Fatalf("first sight: resolvable=%v kept=%v n=%d, want slot:s kept", ok, kept, n)
	}
	clk.now = slotT0.Add(time.Minute)
	moved := slotsSnap(cdcSlot("s", "0/2000", 1000))
	ok, kept, n = subCycle(a, moved, nil, "cdc_consumer_stalled")
	if !ok || n != 0 || len(kept) != 0 {
		t.Fatalf("after a move: resolvable=%v kept=%v n=%d, want it to resolve", ok, kept, n)
	}
	unread := &collector.Snapshot{Unavailable: map[string]string{"replication": "timeout"}}
	if ok, _, _ := subCycle(a, unread, nil, "cdc_consumer_stalled"); ok {
		t.Fatal("unread replication: resolvable, want left untouched")
	}
}

func heartbeatSnap(at time.Time, rs ...collector.CDCHeartbeatReading) *collector.Snapshot {
	return &collector.Snapshot{CollectedAt: at, CDCHeartbeats: rs}
}

func reading(slot string, age time.Duration) collector.CDCHeartbeatReading {
	return collector.CDCHeartbeatReading{Slot: slot, Table: "cdc.heartbeat", Column: "ts",
		MaxAge: 2 * time.Minute, Latest: timePtr(slotT0.Add(-age))}
}

func TestCDCHeartbeat_StaleBoundary(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want int
	}{{time.Minute, 0}, {2*time.Minute - time.Second, 0}, {2 * time.Minute, 1}, {time.Hour, 1}} {
		snap := heartbeatSnap(slotT0, reading("debezium", tc.age))
		got := ruleCDCHeartbeatStale(snap, nil, phase2Config(), &RuleExtras{})
		if len(got) != tc.want {
			t.Fatalf("age %s: %d findings, want %d", tc.age, len(got), tc.want)
		}
		if tc.want == 0 {
			continue
		}
		f := got[0]
		if f.Category != "cdc_heartbeat_stale" || f.ObjectIdentifier != "cdc_heartbeat:debezium" ||
			f.RecommendedSQL != "" || f.Detail["age_seconds"] != tc.age.Seconds() ||
			f.Detail["table"] != "cdc.heartbeat" {
			t.Fatalf("finding = %+v, want an advisory stale heartbeat with its age", f)
		}
	}
}

// A heartbeat table with no rows has never been written: stale.
func TestCDCHeartbeat_NoRowsIsStale(t *testing.T) {
	r := reading("s", 0)
	r.Latest = nil
	got := ruleCDCHeartbeatStale(heartbeatSnap(slotT0, r), nil, phase2Config(), &RuleExtras{})
	if len(got) != 1 || got[0].Detail["no_rows"] != true {
		t.Fatalf("empty heartbeat table: %+v, want one finding with no_rows", got)
	}
}

// Error propagation: a heartbeat that could not be read is unknown, not
// fresh: no finding, and its open finding is kept.
func TestCDCHeartbeat_ReadErrorHoldsOpenFinding(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	a := slotHoldAnalyzer(clk)
	bad := reading("broken", 0)
	bad.Latest, bad.Err = nil, `relation "cdc.heartbeat" does not exist`
	ok, kept, n := subCycle(a, heartbeatSnap(slotT0, bad, reading("fine", 0)), nil,
		"cdc_heartbeat_stale")
	if !ok || n != 0 || len(kept) != 1 || kept[0] != "cdc_heartbeat:broken" {
		t.Fatalf("read error: resolvable=%v kept=%v n=%d, want cdc_heartbeat:broken kept",
			ok, kept, n)
	}
}

// CHECK-07: no finding of the new rules carries executable SQL.
func TestMarket13Rules_NoRecommendedSQL(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	cfg := phase2Config()
	ruleSubscriptionErrors(subSnap(slotT0, liveSub("a", 0)), nil, cfg, ex)
	disabled := liveSub("b", 2)
	disabled.Enabled, disabled.WorkerPID = false, nil
	lagging := liveSub("c", 0)
	lagging.LatestEndTime = timePtr(slotT0.Add(-time.Hour))
	cur := subSnap(slotT0, liveSub("a", 3), disabled, lagging)
	var all []Finding
	all = append(all, ruleSubscriptionErrors(cur, nil, cfg, ex)...)
	all = append(all, ruleSubscriptionLag(cur, cur, cfg, ex)...)
	stallFindings(slotsSnap(cdcSlot("s", "0/1", 1)), ex)
	clk.now = slotT0.Add(time.Hour)
	all = append(all, stallFindings(slotsSnap(cdcSlot("s", "0/1", 99)), ex)...)
	all = append(all, ruleCDCHeartbeatStale(heartbeatSnap(slotT0, reading("h", time.Hour)),
		nil, cfg, ex)...)
	seen := map[string]bool{}
	for _, f := range all {
		seen[f.Category] = true
		if f.RecommendedSQL != "" {
			t.Errorf("%s %s carries RecommendedSQL %q", f.Category, f.ObjectIdentifier,
				f.RecommendedSQL)
		}
	}
	for _, c := range []string{"subscription_errors", "subscription_lag",
		"cdc_consumer_stalled", "cdc_heartbeat_stale"} {
		if !seen[c] {
			t.Errorf("premise: no %s finding produced", c)
		}
	}
}
