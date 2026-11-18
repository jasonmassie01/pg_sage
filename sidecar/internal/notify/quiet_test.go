package notify

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ux-09b: per-channel quiet hours. A channel's own window holds its Ask
// and Digest deliveries until the window ends; a Page goes out at once.
// The global alerting pair stays the default for channels without one.
// No concurrent access test here: a QuietWindow is an immutable value and
// the channel is read per delivery (the outbox's concurrency is covered in
// outbox_db_test.go).

func mustQuiet(t *testing.T, start, end, tz string) QuietWindow {
	t.Helper()
	w, err := ParseQuietHours(start, end, tz)
	if err != nil {
		t.Fatalf("ParseQuietHours(%q, %q, %q): %v", start, end, tz, err)
	}
	return w
}

func TestParseQuietHours_RefusesBadInput(t *testing.T) {
	cases := []struct{ start, end, tz, want string }{
		{"22:00", "22:00", "UTC", "equal"},
		{"07:00", "07:00", "America/Chicago", "equal"},
		{"22:00", "07:00", "Mars/Base", `"Mars/Base"`},
		{"25:00", "07:00", "UTC", `"25:00"`},
		{"22:00", "7am", "UTC", `"7am"`},
		{"22:00", "", "UTC", "both"},
		{"", "07:00", "UTC", "both"},
		{"", "", "Not/AZone", `"Not/AZone"`},
	}
	for _, c := range cases {
		_, err := ParseQuietHours(c.start, c.end, c.tz)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseQuietHours(%q, %q, %q) error %v, want it to name %s",
				c.start, c.end, c.tz, err, c.want)
		}
	}
}

func TestParseQuietHours_EmptyIsNoWindow(t *testing.T) {
	w, err := ParseQuietHours("", "", "")
	if err != nil || w.Set() {
		t.Fatalf("empty quiet hours = %+v, %v; want no window", w, err)
	}
	at := stateT0.Add(11 * time.Hour)
	if got := w.Until(at); !got.Equal(at) {
		t.Fatalf("no window held %v until %v", at, got)
	}
	var zero QuietWindow
	if zero.Set() || !zero.Until(at).Equal(at) {
		t.Fatal("the zero QuietWindow must hold nothing")
	}
}

func TestParseQuietHours_DefaultsToUTCAndCrossesMidnight(t *testing.T) {
	w := mustQuiet(t, "22:00", "07:00", "")
	if !w.Set() {
		t.Fatal("22:00-07:00 is a window")
	}
	at := time.Date(2026, 11, 12, 23, 0, 0, 0, time.UTC)
	want := time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)
	if got := w.Until(at); !got.Equal(want) {
		t.Fatalf("23:00 UTC held until %v, want %v", got, want)
	}
	outside := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	if got := w.Until(outside); !got.Equal(outside) {
		t.Fatalf("noon was held until %v", got)
	}
	// The window's end is outside it.
	if got := w.Until(want); !got.Equal(want) {
		t.Fatalf("07:00 itself was held until %v", got)
	}
}

// Boundary: the nights the clocks change in America/Chicago. The message
// is held for the right local hours: until 07:00 local, however long the
// night is.
func TestQuietWindow_DSTNightsInChicago(t *testing.T) {
	w := mustQuiet(t, "22:00", "07:00", "America/Chicago")
	// Fall back, 2026-11-01: 23:00 CDT on Oct 31 is 04:00 UTC; 07:00 CST
	// is 13:00 UTC, a nine-hour night.
	fall := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)
	if got, want := w.Until(fall), time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC); !got.
		Equal(want) {
		t.Fatalf("fall-back night held until %v, want %v", got, want)
	}
	// 06:30 CST after the change is still inside.
	late := time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)
	if got := w.Until(late); !got.Equal(time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("06:30 CST held until %v", got)
	}
	// Spring forward, 2026-03-08: 23:00 CST on Mar 7 is 05:00 UTC; 07:00
	// CDT is 12:00 UTC, a seven-hour night.
	spring := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)
	if got, want := w.Until(spring), time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC); !got.
		Equal(want) {
		t.Fatalf("spring-forward night held until %v, want %v", got, want)
	}
}

// quietRig is a class rig whose channel has its own quiet hours.
func newQuietRig(t *testing.T, start, end, tz string) *classRig {
	t.Helper()
	rig := newClassRig(t, 15*time.Minute)
	ch, err := rig.store.Channel(context.Background(), rig.ch)
	if err != nil {
		t.Fatal(err)
	}
	ch.QuietStart, ch.QuietEnd, ch.QuietTZ = start, end, tz
	rig.store.AddChannel(*ch)
	return rig
}

func digestReady(db string) Event {
	return Event{Type: EventDigestReady, Severity: "info", Subject: "Daily digest",
		Data: map[string]any{"database": db}, DedupKey: "digest:" + db}
}

// chicago23 moves the rig's clock (stateT0, 12:00 UTC on 2026-11-12) to
// 23:00 CST that day, 05:00 UTC on 2026-11-13.
func chicago23(rig *classRig) { rig.clock.Advance(17 * time.Hour) }

func TestChannelQuiet_HoldsADigestAndSendsAPageAtOnce(t *testing.T) {
	rig := newQuietRig(t, "22:00", "07:00", "America/Chicago")
	chicago23(rig)
	rig.submit(t, digestReady("orders"))
	rig.submit(t, criticalEvent())
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 || !strings.Contains(got[0], "Critical") {
		t.Fatalf("at 23:00 sent %q, want only the page", got)
	}
	// The open page reminds every 4 h (testPolicy); only the digest waits.
	rig.clock.Advance(7*time.Hour + 59*time.Minute) // 06:59 CST
	rig.drain(t)
	if got := rig.subjects(); countSubject(got, "Daily digest") != 0 {
		t.Fatalf("the digest went out before 07:00: %q", got)
	}
	rig.clock.Advance(time.Minute) // 07:00 CST
	rig.drain(t)
	if got := rig.subjects(); countSubject(got, "Daily digest") != 1 {
		t.Fatalf("at 07:00 sent %q, want the held digest once", got)
	}
}

func countSubject(subjects []string, want string) int {
	n := 0
	for _, s := range subjects {
		if s == want {
			n++
		}
	}
	return n
}

// sentTypes is the event type of every sent delivery in the store's log.
func sentTypes(s *MemoryStore) []string {
	var out []string
	for _, d := range s.Deliveries() {
		if d.Status == "sent" {
			out = append(out, d.Event.Type)
		}
	}
	return out
}

func TestChannelQuiet_HoldsAnAskBatchPastItsWindow(t *testing.T) {
	rig := newQuietRig(t, "22:00", "07:00", "America/Chicago")
	chicago23(rig)
	rig.submit(t, askEvent("orders", 1))
	rig.clock.Advance(15 * time.Minute)
	rig.drain(t)
	if got := rig.subjects(); len(got) != 0 {
		t.Fatalf("an Ask went out in quiet hours: %q", got)
	}
	rig.clock.Advance(8 * time.Hour)
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 || !strings.Contains(got[0], "Index 1") {
		t.Fatalf("after quiet hours sent %q, want the Ask", got)
	}
}

// The global pair stays the default: without its own window a channel
// keeps today's behaviour (finding alerts wait, the digest does not).
func TestChannelQuiet_GlobalPairIsTheDefault(t *testing.T) {
	rig := newOutboxRig(t, "22:00", "07:00")
	rig.slackChannelOn(t, "finding_opened", EventDigestReady)
	rig.clock.Advance(11 * time.Hour) // 23:00 UTC
	submitAll(t, rig.d, findingEvt("warning"), digestReady("orders"))
	rig.drain(t)
	if got := sentTypes(rig.store); len(got) != 1 || got[0] != EventDigestReady {
		t.Fatalf("at 23:00 sent %v, want only the digest", got)
	}
	rig.clock.Advance(8 * time.Hour)
	rig.drain(t)
	if got := sentTypes(rig.store); len(got) != 2 || got[1] != "finding_opened" {
		t.Fatalf("at 07:00 sent %v, want the held finding", got)
	}
}

// A channel's own window replaces the global pair for its finding alerts.
func TestChannelQuiet_ChannelWindowReplacesTheGlobalPair(t *testing.T) {
	rig := newOutboxRig(t, "22:00", "07:00")
	id := rig.slackChannelOn(t, "finding_opened")
	ch, _ := rig.store.Channel(context.Background(), id)
	ch.QuietStart, ch.QuietEnd, ch.QuietTZ = "01:00", "02:00", "UTC"
	rig.store.AddChannel(*ch)
	rig.clock.Advance(11 * time.Hour) // 23:00 UTC: global quiet, channel not
	submitAll(t, rig.d, findingEvt("warning"))
	if res := rig.drain(t); res.Sent != 1 {
		t.Fatalf("drained %+v, want the finding sent under the channel's own window", res)
	}
}

// A stored window that no longer parses holds nothing: delivery wins.
func TestChannelQuiet_InvalidStoredWindowDoesNotHold(t *testing.T) {
	rig := newQuietRig(t, "22:00", "22:00", "UTC")
	rig.clock.Advance(10*time.Hour + 30*time.Minute) // 22:30 UTC
	rig.submit(t, digestReady("orders"))
	rig.drain(t)
	if got := rig.subjects(); len(got) != 1 {
		t.Fatalf("an invalid window held the digest: %q", got)
	}
}

// slackChannelOn adds a Slack channel that answers 200 with rules for
// events, and returns its id.
func (r *outboxRig) slackChannelOn(t *testing.T, events ...string) int {
	t.Helper()
	seq := &statusSeq{statuses: []int{200}}
	return r.slackChannel(seq.server(t).URL, events...)
}

func submitAll(t *testing.T, d *Dispatcher, evts ...Event) {
	t.Helper()
	for _, evt := range evts {
		if _, err := d.Submit(context.Background(), evt); err != nil {
			t.Fatalf("Submit(%s): %v", evt.Type, err)
		}
	}
}
