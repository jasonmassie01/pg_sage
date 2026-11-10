package lakebaseobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// streakFixture is a plans-only runtime over n pending plan events, a second
// apart, whose consumer fails each event for which fail returns true.
func streakFixture(t *testing.T, name string, n int, now time.Time,
	fail func(i int) bool,
) (*Runtime, *Store, *recordingConsumer, []Event) {
	t.Helper()
	o := testOptions()
	o.Streams = []Stream{Plans}
	o.Window = SourceRetention
	var events []Event
	c := newRecording(name, nil)
	rt, s := runtimeStore(t, sliceReader(&events), o, c)
	index := map[string]int{}
	for i := range n {
		e := deliveryEvent(t, rt.Scope(), Plans, now.Add(-time.Hour+time.Duration(i)*time.Second))
		events = append(events, e)
		index[e.ID] = i
	}
	c.fail = func(e Event) error {
		if fail(index[e.ID]) {
			// disk_full is sink-wide but not classified as an outage.
			return &pgconn.PgError{Code: "53100"}
		}
		return nil
	}
	return rt, s, c, events
}

// A sink-wide fault the classifier does not know must not spend an attempt
// on every pending event in one poll, or a few polls of it would acknowledge
// the whole backlog as delivery_failed.
func TestDBConsecutiveFailuresStopThePollAtTheStreakLimit(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	n := maxFailureStreak + 2
	rt, s, c, events := streakFixture(t, "streak.v1", n, now, func(int) bool { return true })
	err := rt.Poll(ctx, now)
	if err == nil || !strings.Contains(err.Error(), "consecutive") {
		t.Fatalf("a failure streak must stop the poll with its own error, got %v", err)
	}
	for i, e := range events {
		want := 0
		if i < maxFailureStreak {
			want = 1
		}
		got, _ := attemptRow(t, s, rt.Binding(), e.ID, "streak.v1")
		if c.delivered[e.ID] != want || got != want {
			t.Fatalf("event %d: delivered=%d attempts=%d, want %d", i, c.delivered[e.ID], got,
				want)
		}
	}
	// Before the first backoff ends the failed events are skipped, so the
	// next poll reaches the events behind them.
	if err = rt.Poll(ctx, now.Add(10*time.Second)); err == nil {
		t.Fatal("failed deliveries not reported")
	}
	for i, e := range events {
		got, class := attemptRow(t, s, rt.Binding(), e.ID, "streak.v1")
		if c.delivered[e.ID] != 1 || got != 1 || class != "sqlstate_53100" {
			t.Fatalf("event %d after second poll: delivered=%d attempts=%d class=%q", i,
				c.delivered[e.ID], got, class)
		}
	}
	if acks := ackCount(t, s, rt.Binding(), "streak.v1"); acks != 0 {
		t.Fatalf("failing sink acknowledged %d events", acks)
	}
}

// Isolated failures, even maxFailureStreak-1 in a row, never stop delivery.
func TestDBFailuresBetweenSuccessesNeverTripTheStreak(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	n := 3 * maxFailureStreak
	// Every maxFailureStreak-th event succeeds; the rest fail.
	fail := func(i int) bool { return (i+1)%maxFailureStreak != 0 }
	rt, s, c, events := streakFixture(t, "isolated.v1", n, now, fail)
	err := rt.Poll(ctx, now)
	if err == nil || strings.Contains(err.Error(), "consecutive") {
		t.Fatalf("want a per-event failure report, not a streak stop: %v", err)
	}
	successes := 0
	for i, e := range events {
		got, _ := attemptRow(t, s, rt.Binding(), e.ID, "isolated.v1")
		wantAttempts := 0
		if fail(i) {
			wantAttempts = 1
		} else {
			successes++
		}
		if c.delivered[e.ID] != 1 || got != wantAttempts || c.sink[e.ID] == fail(i) {
			t.Fatalf("event %d: delivered=%d attempts=%d sink=%v", i, c.delivered[e.ID], got,
				c.sink[e.ID])
		}
	}
	if acks := ackCount(t, s, rt.Binding(), "isolated.v1"); acks != successes {
		t.Fatalf("acks = %d, want %d", acks, successes)
	}
}
