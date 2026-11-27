package notify

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// alerting-06 review: the reminder scan lists only the open alerts whose
// own severity has a repeat interval that has passed. Open alerts of a
// severity that never reminds (info by default) sit in the state for as
// long as their finding stays open, oldest first; they must not fill the
// bounded scan and starve the reminders that are due.
// No concurrent access tests here: the scan's locking is the store's,
// covered by TestOutbox_TwoDatabasesSameKeyHaveIndependentState.

// addOpenStates stores n open alerts of severity sev on channel ch, last
// delivered at last.
func addOpenStates(m *MemoryStore, ch, n int, sev string, last time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < n; i++ {
		key := StateKey{Database: "db1", ChannelID: ch,
			DedupKey: fmt.Sprintf("finding:db1:%s:%d", sev, i)}
		m.states[key] = NotifyState{Key: key, Open: true, Severity: sev,
			FirstSentAt: last, LastSentAt: last, Payload: []byte(`{"type":"finding_opened"}`)}
	}
}

func TestOutbox_NeverRemindingAlertsDoNotStarveDueReminders(t *testing.T) {
	rig := newOutboxRig(t, "", "")
	seq := &statusSeq{statuses: []int{200}}
	ch := rig.slackChannel(seq.server(t).URL, "finding_opened")
	addOpenStates(rig.store, ch, reminderScanLimit+50, "info", stateT0.Add(-72*time.Hour))
	if _, err := rig.d.Submit(context.Background(), findingEvt("warning")); err != nil {
		t.Fatal(err)
	}
	rig.drain(t)
	rig.clock.Advance(60 * time.Minute) // the warning's repeat interval
	if got := rig.drain(t); got.Reminders != 1 {
		t.Fatalf("drain at the interval = %+v, want the warning's reminder queued", got)
	}
	rig.drain(t)
	if _, accepted := seq.counts(); accepted != 2 {
		t.Fatalf("accepted = %d, want the alert and its reminder only", accepted)
	}
}

func TestMemoryStoreDueReminders_PerSeverityCutoffs(t *testing.T) {
	m := NewMemoryStore()
	addOpenStates(m, 1, 2, "info", stateT0.Add(-72*time.Hour))
	addOpenStates(m, 1, 1, "warning", stateT0.Add(-61*time.Minute))
	addOpenStates(m, 1, 1, "critical", stateT0.Add(-3*time.Hour))
	cutoffs := map[string]time.Time{"warning": stateT0.Add(-60 * time.Minute),
		"critical": stateT0.Add(-4 * time.Hour)}
	due, err := m.DueReminders(context.Background(), cutoffs, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Severity != "warning" {
		t.Fatalf("due = %+v, want only the warning past its interval", due)
	}
	// Boundary: a state exactly at its cutoff is due; the limit bounds it.
	cutoffs["critical"] = stateT0.Add(-3 * time.Hour)
	if due, _ = m.DueReminders(context.Background(), cutoffs, 1); len(due) != 1 ||
		due[0].Severity != "critical" {
		t.Fatalf("limited scan = %+v, want the oldest due state (critical)", due)
	}
	// Nil/empty: no severity reminds, nothing is listed.
	if due, _ = m.DueReminders(context.Background(), nil, 10); len(due) != 0 {
		t.Fatalf("scan without cutoffs = %+v, want none", due)
	}
}

func TestAlertPolicyReminderCutoffs_OnlySeveritiesThatRemind(t *testing.T) {
	p := testPolicy(t, "", "")
	got := p.reminderCutoffs(stateT0)
	want := map[string]time.Time{"critical": stateT0.Add(-240 * time.Minute),
		"warning": stateT0.Add(-60 * time.Minute)}
	if len(got) != len(want) {
		t.Fatalf("cutoffs = %v, want %v", got, want)
	}
	for sev, at := range want {
		if !got[sev].Equal(at) {
			t.Errorf("cutoff %s = %v, want %v", sev, got[sev], at)
		}
	}
	never, _ := NewAlertPolicy(0, map[string]int{"critical": 0, "warning": 0, "info": 0},
		"", "", "")
	if c := never.reminderCutoffs(stateT0); len(c) != 0 {
		t.Fatalf("cutoffs with no repeat = %v, want none", c)
	}
}
