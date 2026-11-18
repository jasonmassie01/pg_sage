package notify

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ux-09b: per-database mutes. A mute silences one class (or every class)
// of one database until it ends; the Page messages for the emergency stop,
// a failed rollback and a freeze red line ignore mutes and quiet hours.

func TestValidateMute_RefusesBadMutes(t *testing.T) {
	now := stateT0
	ok := Mute{Database: "orders", Until: now.Add(time.Hour), Actor: "admin@example.com"}
	cases := []struct {
		name string
		edit func(*Mute)
		want string
	}{
		{"no until", func(m *Mute) { m.Until = time.Time{} }, "until"},
		{"until in the past", func(m *Mute) { m.Until = now.Add(-time.Second) }, "future"},
		{"until now", func(m *Mute) { m.Until = now }, "future"},
		{"longer than 90 days", func(m *Mute) { m.Until = now.Add(MaxMuteDuration + time.Second) },
			"90 days"},
		{"no database", func(m *Mute) { m.Database = " " }, "database"},
		{"unknown class", func(m *Mute) { m.Class = "pager" }, "pager"},
		{"no actor", func(m *Mute) { m.Actor = "" }, "actor"},
		{"long reason", func(m *Mute) { m.Reason = strings.Repeat("x", 501) }, "reason"},
	}
	for _, c := range cases {
		m := ok
		c.edit(&m)
		if err := ValidateMute(m, now); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want it to name %q", c.name, err, c.want)
		}
	}
	for _, class := range []string{"", "page", "ask", "digest"} {
		m := ok
		m.Class = class
		if err := ValidateMute(m, now); err != nil {
			t.Errorf("class %q refused: %v", class, err)
		}
	}
	m := ok
	m.Until = now.Add(MaxMuteDuration) // exactly 90 days is allowed
	if err := ValidateMute(m, now); err != nil {
		t.Fatalf("a 90-day mute was refused: %v", err)
	}
	if MaxMuteDuration != 90*24*time.Hour {
		t.Fatalf("MaxMuteDuration = %v, want 90 days", MaxMuteDuration)
	}
}

// CHECK-02 (part 1): the never-muted Page list is pinned.
func TestNeverMuted_ListIsPinned(t *testing.T) {
	want := []string{PageEmergencyStop, PageFreezeRedLine, PageRollbackFailed}
	if got := NeverMutedReasons(); !reflect.DeepEqual(got, want) {
		t.Fatalf("NeverMutedReasons() = %v, want %v", got, want)
	}
	if PageEmergencyStop != "emergency_stop" || PageFreezeRedLine != "freeze_red_line" ||
		PageRollbackFailed != "rollback_failed" {
		t.Fatal("the never-muted reasons were renamed")
	}
	for _, evt := range neverMutedEvents() {
		if !NeverMuted(evt) || ClassOf(evt) != ClassPage {
			t.Errorf("%s %v: NeverMuted=%v class=%s, want never muted and Page",
				evt.Type, evt.Data, NeverMuted(evt), ClassOf(evt))
		}
	}
	for _, evt := range []Event{
		criticalEvent(),
		{Type: "action_failed", Severity: "warning", Data: map[string]any{"database": "orders"}},
		{Type: "custodian_escalation", Severity: "warning",
			Data: map[string]any{"database": "orders", "feature": "freeze_blocker"}},
		{Type: "custodian_escalation", Severity: "warning",
			Data: map[string]any{"database": "orders", "feature": "vacuum",
				"profile": "emergency"}},
		{Type: "incident_detected", Severity: "warning",
			Data: map[string]any{"database": "orders", DataPageReason: "something_else"}},
		{},
	} {
		if NeverMuted(evt) {
			t.Errorf("%s %v is never-muted, want mutable", evt.Type, evt.Data)
		}
	}
}

// neverMutedEvents is one event for each entry of the never-muted list.
func neverMutedEvents() []Event {
	db := map[string]any{"database": "orders"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range db {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	return []Event{
		{Type: "incident_detected", Severity: "warning", Subject: "Emergency stop engaged",
			Data:     with(map[string]any{DataPageReason: PageEmergencyStop}),
			DedupKey: "estop:orders"},
		{Type: "action_failed", Severity: "warning", Subject: "Rollback failed",
			Data: with(map[string]any{DataRollbackFailed: true}), DedupKey: "rbf:orders"},
		{Type: "incident_detected", Severity: "warning", Subject: "Rollback failed (marked)",
			Data:     with(map[string]any{DataPageReason: PageRollbackFailed}),
			DedupKey: "rbf2:orders"},
		{Type: "custodian_escalation", Severity: "warning", Subject: "Emergency freeze failing",
			Data:     with(map[string]any{"feature": "freeze", "profile": "emergency"}),
			DedupKey: "freeze:orders"},
		{Type: "custodian_escalation", Severity: "warning", Subject: "Freeze failing",
			Data:     with(map[string]any{"feature": "freeze"}),
			DedupKey: "freeze-normal:orders"},
		{Type: "incident_detected", Severity: "warning", Subject: "Freeze red line",
			Data:     with(map[string]any{DataPageReason: PageFreezeRedLine}),
			DedupKey: "redline:orders"},
	}
}

func (r *classRig) mute(t *testing.T, db, class string, until time.Time) {
	t.Helper()
	if _, err := r.store.PutMute(context.Background(), Mute{Database: db, Class: class,
		Until: until, Actor: "admin@example.com", Reason: "noisy"}); err != nil {
		t.Fatalf("PutMute: %v", err)
	}
}

func TestMute_DatabaseMuteSilencesEveryClassOfThatDatabaseOnly(t *testing.T) {
	rig := newClassRig(t, 0)
	rig.mute(t, "orders", "", rig.clock.Now().Add(time.Hour))
	res, outs, err := rig.d.SubmitWithOutcomes(context.Background(), criticalEvent())
	if err != nil || res.Suppressed != 1 || res.Enqueued != 0 {
		t.Fatalf("muted page: %+v, %v", res, err)
	}
	if len(outs) != 1 || outs[0].Status != OutcomeMuted {
		t.Fatalf("muted page outcomes %+v, want one %q", outs, OutcomeMuted)
	}
	rig.submit(t, askEvent("orders", 1))
	rig.submit(t, digestReady("orders"))
	rig.submit(t, askEvent("billing", 2))
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 || !strings.Contains(got[0], "Index 2") {
		t.Fatalf("sent %q, want only billing's ask", got)
	}
}

func TestMute_ClassMuteLeavesTheOtherClasses(t *testing.T) {
	rig := newClassRig(t, 0)
	rig.mute(t, "orders", "ask", rig.clock.Now().Add(time.Hour))
	rig.submit(t, askEvent("orders", 1))
	rig.submit(t, criticalEvent())
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 || !strings.Contains(got[0], "Critical") {
		t.Fatalf("sent %q, want only the page", got)
	}
}

// CHECK-02: every never-muted Page goes out at once under a database mute
// and the channel's quiet hours.
func TestMute_NeverMutedPagesIgnoreMutesAndQuietHours(t *testing.T) {
	for _, evt := range neverMutedEvents() {
		rig := newQuietRig(t, "00:00", "23:59", "UTC")
		rig.mute(t, "orders", "", rig.clock.Now().Add(time.Hour))
		rig.mute(t, "orders", "page", rig.clock.Now().Add(time.Hour))
		rig.store.AddRule(Rule{ChannelID: rig.ch, Event: evt.Type, MinSeverity: "info",
			Enabled: true})
		rig.submit(t, evt)
		rig.drain(t)
		if got := rig.subjects(); len(got) != 1 || got[0] != evt.Subject {
			t.Errorf("%s: sent %q, want %q at once", evt.Subject, got, evt.Subject)
		}
	}
}

// Boundary: a mute ending at 12:00:00 lets a 12:00:00 message through.
func TestMute_EndsAtItsUntilInstant(t *testing.T) {
	rig := newClassRig(t, 0)
	end := rig.clock.Now().Add(time.Hour)
	rig.mute(t, "orders", "", end)
	rig.clock.Advance(time.Hour - time.Second)
	rig.submit(t, criticalEvent())
	rig.drain(t)
	if got := rig.subjects(); len(got) != 0 {
		t.Fatalf("a second before the end sent %q", got)
	}
	rig.clock.Advance(time.Second)
	if !rig.clock.Now().Equal(end) {
		t.Fatal("clock is not at the mute's end")
	}
	rig.submit(t, Event{Type: "finding_critical", Severity: "critical", Subject: "At the end",
		Data: map[string]any{"database": "orders"}, DedupKey: "end:orders"})
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 || got[0] != "At the end" {
		t.Fatalf("at the mute's end sent %q, want the message", got)
	}
}

// State transitions: mute, unmute, deliver; an expired mute is ignored
// although its row is still stored (no sweep).
func TestMute_MuteUnmuteThenDeliverAndExpiry(t *testing.T) {
	rig := newClassRig(t, 0)
	ctx := context.Background()
	rig.mute(t, "orders", "", rig.clock.Now().Add(time.Hour))
	rig.submit(t, askEvent("orders", 1))
	removed, err := rig.store.DeleteMute(ctx, "orders", "")
	if err != nil || !removed {
		t.Fatalf("DeleteMute = %v, %v", removed, err)
	}
	if again, err := rig.store.DeleteMute(ctx, "orders", ""); err != nil || again {
		t.Fatalf("a second DeleteMute = %v, %v; want nothing removed", again, err)
	}
	rig.submit(t, askEvent("orders", 2))
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 || !strings.Contains(got[0], "Index 2") {
		t.Fatalf("after unmute sent %q, want only the second ask", got)
	}
	rig.mute(t, "orders", "", rig.clock.Now().Add(time.Minute))
	rig.clock.Advance(2 * time.Minute)
	all, err := rig.store.ListMutes(ctx, time.Time{})
	if err != nil || len(all) != 1 {
		t.Fatalf("stored mutes %+v, %v; want the expired row kept", all, err)
	}
	active, err := rig.store.ListMutes(ctx, rig.clock.Now())
	if err != nil || len(active) != 0 {
		t.Fatalf("active mutes %+v, %v; want none", active, err)
	}
	rig.submit(t, askEvent("orders", 3))
	rig.drain(t)
	if got := rig.subjects(); len(got) != 2 {
		t.Fatalf("an expired mute held the ask: %q", got)
	}
}

// failingMutes is a store whose mute read fails.
type failingMutes struct{ *MemoryStore }

func (f failingMutes) ActiveMute(context.Context, string, Class, time.Time) (bool, error) {
	return false, errors.New("relation sage.notification_mutes does not exist")
}

// Error propagation: a failed mute read sends the message (fail open
// toward delivery), logs the failure and counts it.
func TestMute_ReadFailureSendsLogsAndCounts(t *testing.T) {
	store := NewMemoryStore()
	var mu sync.Mutex
	var logs []string
	logFn := func(level, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, level+" "+fmt.Sprintf(format, args...))
	}
	d := NewDefaultDispatcher(failingMutes{store}, TargetPolicy{AllowPrivate: true}, logFn)
	seq := &statusSeq{statuses: []int{200}}
	id := store.AddChannel(Channel{Name: "hook", Type: "webhook",
		Config: map[string]string{"webhook_url": seq.server(t).URL}, Enabled: true})
	store.AddRule(Rule{ChannelID: id, Event: "finding_critical", MinSeverity: "info",
		Enabled: true})
	before := d.MuteReadFailures()
	res, err := d.Submit(context.Background(), criticalEvent())
	if err != nil || res.Delivered != 1 {
		t.Fatalf("Submit = %+v, %v; want the page delivered", res, err)
	}
	if got := d.MuteReadFailures() - before; got != 1 {
		t.Fatalf("mute read failures counted %d, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "ERROR") || !strings.Contains(joined, "mute") ||
		!strings.Contains(joined, "orders") || !strings.Contains(joined, "does not exist") {
		t.Fatalf("logs %q do not name the failed mute read", joined)
	}
}

// An event of no database is never muted (a mute names a database).
func TestMute_EventWithoutDatabaseIsNotMuted(t *testing.T) {
	rig := newClassRig(t, 0)
	rig.mute(t, "orders", "", rig.clock.Now().Add(time.Hour))
	rig.submit(t, Event{Type: "finding_critical", Severity: "critical", Subject: "Fleet",
		DedupKey: "fleet"})
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 {
		t.Fatalf("a fleet-wide page sent %q", got)
	}
}

// Concurrency: two admins mute the same database at once; one mute
// results and the later until wins.
func TestMute_ConcurrentMutesKeepOneRowWithTheLaterUntil(t *testing.T) {
	store := NewMemoryStore()
	base := stateT0
	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = store.PutMute(context.Background(), Mute{Database: "orders",
				Until: base.Add(time.Duration(i) * time.Hour), Actor: fmt.Sprintf("a%d", i)})
		}(i)
	}
	wg.Wait()
	all, err := store.ListMutes(context.Background(), time.Time{})
	if err != nil || len(all) != 1 {
		t.Fatalf("mutes %+v, %v; want one row", all, err)
	}
	if !all[0].Until.Equal(base.Add(8*time.Hour)) || all[0].Actor != "a8" {
		t.Fatalf("mute %+v, want until +8h by a8", all[0])
	}
	// An earlier until does not shorten the mute.
	got, err := store.PutMute(context.Background(), Mute{Database: "orders",
		Until: base.Add(time.Hour), Actor: "late"})
	if err != nil || !got.Until.Equal(base.Add(8*time.Hour)) {
		t.Fatalf("a shorter mute returned %+v, %v; want the +8h mute kept", got, err)
	}
}
