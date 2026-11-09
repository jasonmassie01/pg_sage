package analyzer

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent-access tests: the analyzer runs its snapshot rules one
// after another on its own goroutine, so the inactive-slot clock in
// RuleExtras is never shared between goroutines.

var slotT0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// slotClock is a movable rule clock.
type slotTestClock struct{ now time.Time }

func (c *slotTestClock) extras() *RuleExtras {
	return &RuleExtras{Now: func() time.Time { return c.now }}
}

func deadSlot(name string) collector.SlotInfo {
	return collector.SlotInfo{
		SlotName: name, SlotType: "logical", Active: false,
		RetainedBytes: 64 << 20,
	}
}

func strPtr(s string) *string { return &s }

func timePtr(t time.Time) *time.Time { return &t }

func slotsSnap(slots ...collector.SlotInfo) *collector.Snapshot {
	return &collector.Snapshot{
		Replication: &collector.ReplicationStats{Slots: slots},
	}
}

// inactiveFindings runs the slot rule and keeps only inactive_slot.
func inactiveFindings(
	snap *collector.Snapshot, cfg *config.Config, ex *RuleExtras,
) []Finding {
	var out []Finding
	for _, f := range ruleInactiveSlots(snap, nil, cfg, ex) {
		if f.Category == "inactive_slot" {
			out = append(out, f)
		}
	}
	return out
}

func TestInactiveSlot_ClockBoundary(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	snap := slotsSnap(deadSlot("debezium"))
	steps := []struct {
		after time.Duration
		want  int
	}{
		{0, 0},
		{4 * time.Minute, 0},
		{5*time.Minute - time.Second, 0},
		{5 * time.Minute, 1},
		{6 * time.Minute, 1},
	}
	for _, st := range steps {
		clk.now = slotT0.Add(st.after)
		got := inactiveFindings(snap, nil, ex)
		if len(got) != st.want {
			t.Fatalf("inactive %s: %d findings, want %d", st.after, len(got), st.want)
		}
		if st.want == 1 {
			f := got[0]
			if f.Severity != "warning" || f.ObjectIdentifier != "slot:debezium" {
				t.Fatalf("finding = %s %s, want warning slot:debezium",
					f.Severity, f.ObjectIdentifier)
			}
			if secs := f.Detail["inactive_seconds"]; secs != st.after.Seconds() {
				t.Fatalf("inactive_seconds = %v, want %v", secs, st.after.Seconds())
			}
		}
	}
}

func TestInactiveSlot_InactiveSinceOnFirstCycle(t *testing.T) {
	for _, tc := range []struct {
		ago  time.Duration
		want int
	}{{6 * time.Minute, 1}, {4 * time.Minute, 0}} {
		clk := &slotTestClock{now: slotT0}
		s := deadSlot("pg17_slot")
		s.InactiveSince = timePtr(slotT0.Add(-tc.ago))
		got := inactiveFindings(slotsSnap(s), nil, clk.extras())
		if len(got) != tc.want {
			t.Fatalf("inactive_since %s ago: %d findings, want %d", tc.ago, len(got), tc.want)
		}
		if tc.want == 1 && got[0].Detail["inactive_seconds"] != tc.ago.Seconds() {
			t.Fatalf("inactive_seconds = %v, want %v",
				got[0].Detail["inactive_seconds"], tc.ago.Seconds())
		}
	}
}

func TestInactiveSlot_InactiveSinceWithNilExtras(t *testing.T) {
	s := deadSlot("pg17_slot")
	s.InactiveSince = timePtr(time.Now().Add(-time.Hour))
	got := inactiveFindings(slotsSnap(s), nil, nil)
	if len(got) != 1 || got[0].Severity != "warning" {
		t.Fatalf("got %+v, want one warning from inactive_since without extras", got)
	}
}

func TestInactiveSlot_NoClockNoInactiveSince_NoFinding(t *testing.T) {
	// Without extras and without inactive_since the duration is unknown:
	// unknown never fires early.
	got := inactiveFindings(slotsSnap(deadSlot("old")), nil, nil)
	if len(got) != 0 {
		t.Fatalf("got %d findings, want none when the inactive duration is unknown", len(got))
	}
}

func TestInactiveSlot_FutureInactiveSince_NoFinding(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	s := deadSlot("skewed")
	s.InactiveSince = timePtr(slotT0.Add(time.Hour))
	if got := inactiveFindings(slotsSnap(s), nil, clk.extras()); len(got) != 0 {
		t.Fatalf("inactive_since in the future gave %d findings, want none", len(got))
	}
}

func TestInactiveSlot_SyncedSlotOnStandbySkipped(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	s := deadSlot("failover_slot")
	s.Synced = true
	s.InactiveSince = timePtr(slotT0.Add(-24 * time.Hour))
	s.WALStatus = strPtr("lost")
	snap := slotsSnap(s)
	snap.System.IsReplica = true
	for i := 0; i < 3; i++ {
		clk.now = slotT0.Add(time.Duration(i) * time.Hour)
		if got := ruleInactiveSlots(snap, nil, nil, ex); len(got) != 0 {
			t.Fatalf("cycle %d: synced standby slot gave %+v, want nothing", i, got)
		}
	}
	if len(ex.InactiveSlotSince) != 0 {
		t.Fatalf("synced slot started a clock: %v", ex.InactiveSlotSince)
	}
}

func TestInactiveSlot_SyncedSlotOnPrimaryReported(t *testing.T) {
	// After a promotion a synchronized slot is a real slot on a primary:
	// an idle one retains WAL and must be reported.
	clk := &slotTestClock{now: slotT0}
	s := deadSlot("promoted_slot")
	s.Synced = true
	s.InactiveSince = timePtr(slotT0.Add(-time.Hour))
	got := inactiveFindings(slotsSnap(s), nil, clk.extras())
	if len(got) != 1 {
		t.Fatalf("synced slot on a primary: %d findings, want 1", len(got))
	}
}

func TestInactiveSlot_TemporarySlotSkipped(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	s := deadSlot("pg_basebackup_123")
	s.Temporary = true
	s.InactiveSince = timePtr(slotT0.Add(-time.Hour))
	s.WALStatus = strPtr("unreserved")
	if got := ruleInactiveSlots(slotsSnap(s), nil, nil, ex); len(got) != 0 {
		t.Fatalf("temporary slot gave %+v, want nothing", got)
	}
}

func TestInactiveSlot_ReactivationResolvesAndRestartsClock(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	idle := slotsSnap(deadSlot("cdc"))
	live := deadSlot("cdc")
	live.Active = true
	active := slotsSnap(live)
	step := func(after time.Duration, snap *collector.Snapshot, want int) {
		t.Helper()
		clk.now = slotT0.Add(after)
		if got := inactiveFindings(snap, nil, ex); len(got) != want {
			t.Fatalf("at +%s: %d findings, want %d", after, len(got), want)
		}
	}
	step(0, idle, 0)
	step(6*time.Minute, idle, 1)
	// Active again: no inactive_slot finding, so the open one resolves.
	step(7*time.Minute, active, 0)
	if _, ok := ex.InactiveSlotSince["cdc"]; ok {
		t.Fatal("an active slot kept its inactive clock")
	}
	// Idle again: the clock restarts, nothing fires early.
	step(8*time.Minute, idle, 0)
	step(12*time.Minute, idle, 0)
	step(13*time.Minute, idle, 1)
	spec := findRuleSpec(t, "inactive_slots")
	if !containsString(spec.Categories, "inactive_slot") || !spec.Needs(active, nil) {
		t.Fatalf("inactive_slots spec %+v must own inactive_slot on replication data", spec)
	}
}

func TestInactiveSlot_SlotGoneForgetsClock(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	inactiveFindings(slotsSnap(deadSlot("a"), deadSlot("b")), nil, ex)
	clk.now = slotT0.Add(time.Minute)
	inactiveFindings(slotsSnap(deadSlot("a")), nil, ex)
	clk.now = slotT0.Add(6 * time.Minute)
	got := inactiveFindings(slotsSnap(deadSlot("a"), deadSlot("b")), nil, ex)
	if len(got) != 1 || got[0].ObjectIdentifier != "slot:a" {
		t.Fatalf("got %+v, want only slot:a (b was dropped and recreated)", got)
	}
}

func TestInactiveSlot_NoReplicationClearsClock(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	ex.InactiveSlotSince = map[string]time.Time{"cdc": slotT0.Add(-time.Hour)}
	if got := ruleInactiveSlots(&collector.Snapshot{}, nil, nil, ex); got != nil {
		t.Fatalf("no replication data gave %+v, want nil", got)
	}
	if got := inactiveFindings(slotsSnap(deadSlot("cdc")), nil, ex); len(got) != 0 {
		t.Fatalf("an unobserved gap must restart the clock, got %+v", got)
	}
}

func TestInactiveSlot_WALStatusFiresAtOnce(t *testing.T) {
	for _, tc := range []struct {
		status, severity string
	}{
		{"lost", "critical"}, {"unreserved", "critical"},
		{"extended", "warning"}, {"reserved", ""},
	} {
		clk := &slotTestClock{now: slotT0}
		s := deadSlot("cdc")
		s.WALStatus = strPtr(tc.status)
		got := inactiveFindings(slotsSnap(s), nil, clk.extras())
		if tc.severity == "" {
			if len(got) != 0 {
				t.Fatalf("wal_status %s on the first cycle gave %+v", tc.status, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Severity != tc.severity {
			t.Fatalf("wal_status %s: got %+v, want one %s", tc.status, got, tc.severity)
		}
		assertManualDrop(t, got[0], "SELECT pg_drop_replication_slot('cdc');")
		if got[0].Detail["wal_status"] != tc.status {
			t.Fatalf("detail wal_status = %v, want %s", got[0].Detail["wal_status"], tc.status)
		}
	}
}

func assertManualDrop(t *testing.T, f Finding, wantSQL string) {
	t.Helper()
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("inactive_slot carries executable SQL %q / %q", f.RecommendedSQL, f.RollbackSQL)
	}
	if f.ActionRisk != "high_risk" {
		t.Fatalf("ActionRisk = %q, want high_risk", f.ActionRisk)
	}
	if got := f.Detail["manual_sql"]; got != wantSQL {
		t.Fatalf("manual_sql = %v, want %s", got, wantSQL)
	}
	reason, _ := f.Detail[DetailApprovalRequired].(string)
	if !strings.Contains(reason, "irreversible") || !strings.Contains(reason, "resync") {
		t.Fatalf("approval_required = %q, want the irreversible/resync reason", reason)
	}
}

func TestInactiveSlot_QuotedNameEscaped(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	name := `o'brien"; DROP TABLE x; --`
	s := deadSlot(name)
	s.WALStatus = strPtr("lost")
	got := inactiveFindings(slotsSnap(s), nil, clk.extras())
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	assertManualDrop(t, got[0],
		`SELECT pg_drop_replication_slot('o''brien"; DROP TABLE x; --');`)
}

func TestInactiveSlot_ConfiguredThreshold(t *testing.T) {
	for _, tc := range []struct {
		minutes int
		after   time.Duration
		want    int
	}{
		{10, 6 * time.Minute, 0}, {10, 10 * time.Minute, 1},
		{0, 5 * time.Minute, 1}, {0, 4 * time.Minute, 0},
		{-3, 4 * time.Minute, 0}, {1, time.Minute, 1},
	} {
		cfg := &config.Config{}
		cfg.Analyzer.InactiveSlotMinMinutes = tc.minutes
		clk := &slotTestClock{now: slotT0}
		s := deadSlot("cdc")
		s.InactiveSince = timePtr(slotT0.Add(-tc.after))
		got := inactiveFindings(slotsSnap(s), cfg, clk.extras())
		if len(got) != tc.want {
			t.Fatalf("min %d, inactive %s: %d findings, want %d",
				tc.minutes, tc.after, len(got), tc.want)
		}
	}
}

func TestInactiveSlot_DetailCarriesSlotFacts(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	s := deadSlot("cdc")
	s.InactiveSince = timePtr(slotT0.Add(-time.Hour))
	s.WALStatus = strPtr("extended")
	safe := int64(1 << 30)
	s.SafeWALSize = &safe
	s.InvalidationReason = strPtr("wal_removed")
	got := inactiveFindings(slotsSnap(s), nil, clk.extras())
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	d := got[0].Detail
	if d["safe_wal_size"] != safe || d["invalidation_reason"] != "wal_removed" ||
		d["slot_type"] != "logical" || d["retained_bytes"] != int64(64<<20) ||
		d["inactive_since"] != slotT0.Add(-time.Hour).Format(time.RFC3339) {
		t.Fatalf("detail = %v, missing slot facts", d)
	}
	if !strings.Contains(got[0].Title, "cdc") || !strings.Contains(got[0].Title, "64.0 MB") {
		t.Fatalf("title = %q, want slot name and retained size", got[0].Title)
	}
}

// CHECK-11: no inactive_slot finding carries executable SQL.
func TestInactiveSlot_NeverRecommendsExecutableSQL(t *testing.T) {
	clk := &slotTestClock{now: slotT0}
	ex := clk.extras()
	var slots []collector.SlotInfo
	for _, st := range []string{"", "reserved", "extended", "unreserved", "lost"} {
		for _, typ := range []string{"logical", "physical"} {
			s := deadSlot(typ + "_" + st)
			s.SlotType = typ
			if st != "" {
				s.WALStatus = strPtr(st)
			}
			s.InactiveSince = timePtr(slotT0.Add(-48 * time.Hour))
			slots = append(slots, s)
		}
	}
	clk.now = slotT0
	got := inactiveFindings(slotsSnap(slots...), nil, ex)
	if len(got) != len(slots) {
		t.Fatalf("got %d inactive findings, want %d", len(got), len(slots))
	}
	for _, f := range got {
		if f.RecommendedSQL != "" {
			t.Fatalf("%s carries RecommendedSQL %q", f.ObjectIdentifier, f.RecommendedSQL)
		}
	}
}

func findRuleSpec(t *testing.T, name string) RuleSpec {
	t.Helper()
	for _, r := range AllRules {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("rule %s not registered", name)
	return RuleSpec{}
}
