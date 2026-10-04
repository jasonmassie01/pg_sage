package earned

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/modellift"
)

// Roadmap 2.4, owner addition A: a family earning or losing model-root
// authority is automatic but never silent. The ledger history records
// each change with the report that decided it, and the operator is told
// through the notification path. The trust page says plainly how many
// more correct held-out overrides each family still needs.

func TestRootAuthorityTransition(t *testing.T) {
	granted := &Event{Type: EventRootAuthorityGranted}
	revoked := &Event{Type: EventRootAuthorityRevoked}
	for _, c := range []struct {
		name    string
		last    *Event
		granted bool
		want    EventType
		changed bool
	}{
		{"never granted, still not", nil, false, "", false},
		{"first grant", nil, true, EventRootAuthorityGranted, true},
		{"still granted", granted, true, "", false},
		{"lost it", granted, false, EventRootAuthorityRevoked, true},
		{"still revoked", revoked, false, "", false},
		{"earned it back", revoked, true, EventRootAuthorityGranted, true},
	} {
		got, changed := rootAuthorityTransition(c.last, c.granted)
		if got != c.want || changed != c.changed {
			t.Errorf("%s: %q %v, want %q %v", c.name, got, changed, c.want, c.changed)
		}
	}
}

func TestRootAuthorityEvidence_NamesTheReport(t *testing.T) {
	a := authority(t, liftReport(benchNow.Add(-time.Hour), "live", false,
		good("lock_blocking", 16, 16)), FamilyLockBlocking, benchNow)
	a.Report.ID = "run-123"
	ev := rootAuthorityEvent(a, EventRootAuthorityGranted, benchNow)
	if ev.Family != FamilyLockBlocking || ev.Class != ModelRootClass ||
		ev.Type != EventRootAuthorityGranted || ev.Actor != ActorPgSage ||
		ev.Reason == "" || len(ev.Reason) > 2000 || !ev.At.Equal(benchNow) {
		t.Fatalf("event = %+v", ev)
	}
	ev2 := decodeEvidence(t, ev.Evidence)
	if ev2["report_id"] != "run-123" || ev2["status"] != RootAdopt ||
		ev2["overrides_needed"] != float64(0) || ev2["threshold"] != 0.8 {
		t.Fatalf("evidence = %v", ev2)
	}
	o, _ := ev2["overrides"].(map[string]any)
	if o["k"] != float64(16) || o["n"] != float64(16) {
		t.Fatalf("overrides = %v", ev2["overrides"])
	}
}

// A revocation for want of any measurement names no report but still
// says why.
func TestRootAuthorityEvidence_RevocationWithoutAReport(t *testing.T) {
	a := authority(t, nil, FamilyWAL, benchNow)
	ev := rootAuthorityEvent(a, EventRootAuthorityRevoked, benchNow)
	got := decodeEvidence(t, ev.Evidence)
	if _, ok := got["report_id"]; ok || ev.Reason == "" ||
		got["overrides_needed"] != float64(16) {
		t.Fatalf("evidence = %v, reason %q", got, ev.Reason)
	}
}

func TestRootAuthority_SaysHowManyMoreCorrectOverridesItNeeds(t *testing.T) {
	at := benchNow.Add(-time.Hour)
	for name, c := range map[string]struct {
		raw  []byte
		want int
	}{
		"not measured": {nil, 16},
		"15 of 15":     {liftReport(at, "live", false, good("lock_blocking", 15, 15)), 1},
		"16 of 16":     {liftReport(at, "live", false, good("lock_blocking", 16, 16)), 0},
		"2 of 20": {liftReport(at, "live", false, good("lock_blocking", 2, 20)),
			modellift.MoreCorrectOverridesNeeded(2, 20)},
		"tuning only": {liftReport(at, "live", false, liftRec("lock_blocking",
			modellift.SplitTuning, 40, 40, [2]int{30, 40}, [2]int{36, 40})), 16},
	} {
		got := authority(t, c.raw, FamilyLockBlocking, benchNow)
		if got.OverridesNeeded != c.want {
			t.Errorf("%s: overrides needed %d, want %d", name, got.OverridesNeeded, c.want)
		}
	}
}

// The need counts the override shortfall only: a family whose counts
// already clear the rule but which another condition holds back needs no
// more overrides, and its reason names the condition.
func TestRootAuthority_NeedIsZeroWhenOnlyAnotherConditionHoldsItBack(t *testing.T) {
	raw := liftReport(benchNow.Add(-time.Hour), "live", false, liftRec("lock_blocking",
		modellift.SplitHeldOut, 40, 40, [2]int{36, 40}, [2]int{35, 40}))
	got := authority(t, raw, FamilyLockBlocking, benchNow)
	if got.Granted || got.OverridesNeeded != 0 || got.Reason == "" {
		t.Fatalf("authority = %+v", got)
	}
}

func decodeEvidence(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("evidence %s: %v", raw, err)
	}
	return m
}
