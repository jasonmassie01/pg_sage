package lakebaseobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// recordingConsumer counts every call the runtime makes into a consumer.
type recordingConsumer struct {
	name      string
	streams   []Stream
	fail      func(Event) error
	mu        sync.Mutex
	delivered map[string]int
	observed  map[string]int
	failed    []string
	sink      map[string]bool
}

func newRecording(name string, streams []Stream) *recordingConsumer {
	return &recordingConsumer{name: name, streams: streams, delivered: map[string]int{},
		observed: map[string]int{}, sink: map[string]bool{}}
}

func (c *recordingConsumer) Name() string      { return c.name }
func (c *recordingConsumer) Streams() []Stream { return c.streams }
func (c *recordingConsumer) Deliver(_ context.Context, e Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delivered[e.ID]++
	if c.fail != nil {
		if err := c.fail(e); err != nil {
			return err
		}
	}
	c.sink[e.ID] = true
	return nil
}
func (c *recordingConsumer) Observe(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observed[e.ID]++
}
func (c *recordingConsumer) DeliveryFailed(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failed = append(c.failed, e.ID)
}
func (c *recordingConsumer) totals() (delivered, observed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.delivered {
		delivered += n
	}
	for _, n := range c.observed {
		observed += n
	}
	return delivered, observed
}

func sliceReader(events *[]Event) readerFunc {
	return func(ctx context.Context, s Stream, sc Scope, w Window) (Batch, error) {
		return staticReader(*events)(ctx, s, sc, w)
	}
}

func attemptRow(t *testing.T, s *Store, binding, eventID, consumer string) (int, string) {
	t.Helper()
	var n int
	var class string
	err := s.pool.QueryRow(context.Background(), `SELECT COALESCE(max(attempts),0),
 COALESCE(max(last_error),'')
FROM sage.lakebaseobs_attempts WHERE binding=$1 AND event_id=$2 AND consumer=$3`,
		binding, eventID, consumer).Scan(&n, &class)
	if err != nil {
		t.Fatal(err)
	}
	return n, class
}

func ackCount(t *testing.T, s *Store, binding, consumer string) int {
	t.Helper()
	var n int
	err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.lakebaseobs_acks
WHERE binding=$1 AND consumer=$2`, binding, consumer).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDBTransientFailuresCapAtFiveAttemptsThenAckDeliveryFailed(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	o := testOptions()
	o.Streams = []Stream{Plans}
	o.Window = SourceRetention
	var events []Event
	c := newRecording("capped.v1", nil)
	rt, s := runtimeStore(t, sliceReader(&events), o, c)
	poison := deliveryEvent(t, rt.Scope(), Plans, now.Add(-2*time.Hour))
	events = append(events, poison)
	c.fail = func(e Event) error {
		if e.ID == poison.ID {
			return errors.New("transient sink failure")
		}
		return nil
	}
	const gap = 20 * time.Minute
	for attempt := 1; attempt <= MaxDeliveryAttempts; attempt++ {
		at := now.Add(time.Duration(attempt-1) * gap)
		late := deliveryEvent(t, rt.Scope(), Plans, at.Add(-10*time.Minute))
		events = append(events, late)
		if err := rt.Poll(ctx, at); err == nil {
			t.Fatalf("attempt %d: failed delivery not reported", attempt)
		}
		if c.delivered[late.ID] != 1 || !c.sink[late.ID] {
			t.Fatalf("attempt %d: later event not delivered between attempts", attempt)
		}
		n, class := attemptRow(t, s, rt.Binding(), poison.ID, "capped.v1")
		if n != attempt || class != "sink_error" {
			t.Fatalf("attempt %d recorded as %d (%q)", attempt, n, class)
		}
		wantAcks := attempt // one per delivered late event
		if attempt == MaxDeliveryAttempts {
			wantAcks++
		}
		if got := ackCount(t, s, rt.Binding(), "capped.v1"); got != wantAcks {
			t.Fatalf("attempt %d: acks = %d, want %d", attempt, got, wantAcks)
		}
	}
	if !slices.Equal(c.failed, []string{poison.ID}) {
		t.Fatalf("delivery_failed must be reported once for the poison event: %v", c.failed)
	}
	if err := rt.Poll(ctx, now.Add(MaxDeliveryAttempts*gap)); err != nil {
		t.Fatalf("an exhausted event must not fail later polls: %v", err)
	}
	if c.delivered[poison.ID] != MaxDeliveryAttempts || c.sink[poison.ID] {
		t.Fatalf("poison event delivered %d times after the cap", c.delivered[poison.ID])
	}
}

func TestDBSinkUnavailableSpendsNoAttemptAndStopsThePoll(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"connection exception", &pgconn.PgError{Code: "08006"}},
		{"admin shutdown", fmt.Errorf("store: %w", &pgconn.PgError{Code: "57P01"})},
		{"sink timeout", fmt.Errorf("store: %w", context.DeadlineExceeded)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)
			o := testOptions()
			o.Streams = []Stream{Plans}
			o.Window = SourceRetention
			var events []Event
			c := newRecording("outage.v1", nil)
			c.fail = func(Event) error { return tc.err }
			rt, s := runtimeStore(t, sliceReader(&events), o, c)
			first := deliveryEvent(t, rt.Scope(), Plans, now.Add(-time.Hour))
			second := deliveryEvent(t, rt.Scope(), Plans, now.Add(-time.Hour+time.Second))
			events = append(events, first, second)
			if err := rt.Poll(ctx, now); err == nil {
				t.Fatal("sink outage not reported")
			}
			if c.delivered[first.ID] != 1 || c.delivered[second.ID] != 0 {
				t.Fatalf("an unavailable sink must stop this poll: %v", c.delivered)
			}
			if n, _ := attemptRow(t, s, rt.Binding(), first.ID, "outage.v1"); n != 0 {
				t.Fatalf("a sink outage spent %d attempts of the event's budget", n)
			}
			pending, err := s.Pending(ctx, rt.Binding(), "outage.v1", 10, now)
			if err != nil || len(pending) != 2 || pending[0].ID != first.ID {
				t.Fatalf("both events must stay pending: %d %v", len(pending), err)
			}
		})
	}
}

func TestDBSerializationFailureIsRetriedPerEvent(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	o := testOptions()
	o.Streams = []Stream{Plans}
	o.Window = SourceRetention
	var events []Event
	c := newRecording("deadlock.v1", nil)
	c.fail = func(Event) error { return &pgconn.PgError{Code: "40P01"} }
	rt, s := runtimeStore(t, sliceReader(&events), o, c)
	events = append(events, deliveryEvent(t, rt.Scope(), Plans, now.Add(-time.Hour)),
		deliveryEvent(t, rt.Scope(), Plans, now.Add(-time.Hour+time.Second)))
	if err := rt.Poll(ctx, now); err == nil {
		t.Fatal("failed deliveries not reported")
	}
	for _, e := range events {
		n, class := attemptRow(t, s, rt.Binding(), e.ID, "deadlock.v1")
		if c.delivered[e.ID] != 1 || n != 1 || class != "sqlstate_40P01" {
			t.Fatalf("event %s: delivered=%d attempts=%d class=%q", e.ID, c.delivered[e.ID],
				n, class)
		}
	}
}

func TestDBMixedStreamsAckNonPlansAtCommitAndDrainPlans(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	streams := []Stream{Plans, Logs, ASH, DDL, DatabaseCounters}
	o := Options{WarehouseID: "test", AllowPayload: true, Streams: streams,
		Window: SourceRetention, MaxRows: 100}
	var events []Event
	c := newRecording("mixed.v1", []Stream{Plans})
	rt, s := runtimeStore(t, sliceReader(&events), o, c)
	events = mixedEvents(t, rt.Scope(), streams, now.Add(-3*time.Hour), 2000)
	plans := len(events) / len(streams)
	polls := 0
	for ; polls < 10; polls++ {
		if err := rt.Poll(ctx, now.Add(time.Duration(polls)*time.Minute)); err != nil {
			t.Fatalf("poll %d: %v", polls+1, err)
		}
		delivered, observed := c.totals()
		if observed != len(events)-plans {
			t.Fatalf("poll %d: observed %d non-plan events, want %d", polls+1, observed,
				len(events)-plans)
		}
		if delivered > plans {
			t.Fatalf("poll %d: %d deliveries for %d plan events", polls+1, delivered, plans)
		}
		d := rt.Deliveries()
		if len(d) != 1 || d[0].Consumer != "mixed.v1" || d[0].PendingRows != plans-delivered {
			t.Fatalf("poll %d: delivery status %+v, want %d pending", polls+1, d, plans-delivered)
		}
		if delivered == plans {
			break
		}
	}
	if polls+1 != plans/o.MaxRows {
		t.Fatalf("backlog drained in %d polls, want %d", polls+1, plans/o.MaxRows)
	}
	assertEachOnce(t, "observed across reconcile re-reads", c.observed)
	assertEachOnce(t, "delivered", c.delivered)
	assertDrained(t, rt, s, c, now.Add(time.Hour), len(events), plans)
}

// assertDrained polls once more and proves nothing is delivered or counted
// again and no backlog remains.
func assertDrained(t *testing.T, rt *Runtime, s *Store, c *recordingConsumer, now time.Time,
	events, plans int,
) {
	t.Helper()
	ctx := context.Background()
	if got := ackCount(t, s, rt.Binding(), "mixed.v1"); got != events {
		t.Fatalf("acks = %d, want %d", got, events)
	}
	if err := rt.Poll(ctx, now); err != nil {
		t.Fatal(err)
	}
	delivered, observed := c.totals()
	if delivered != plans || observed != events-plans {
		t.Fatalf("drained runtime made %d deliveries and %d observations", delivered, observed)
	}
	b, err := s.Backlog(ctx, rt.Binding(), "mixed.v1", []Stream{Plans}, now)
	if err != nil || b.Rows != 0 || b.Capped || !b.Oldest.IsZero() {
		t.Fatalf("drained backlog %+v %v", b, err)
	}
}

// mixedEvents spreads n events evenly over streams, a millisecond apart.
func mixedEvents(t *testing.T, scope Scope, streams []Stream, from time.Time, n int) []Event {
	t.Helper()
	out := make([]Event, 0, n)
	for i := range n {
		at := from.Add(time.Duration(i) * time.Millisecond)
		out = append(out, deliveryEvent(t, scope, streams[i%len(streams)], at))
	}
	return out
}

func assertEachOnce(t *testing.T, what string, counts map[string]int) {
	t.Helper()
	for id, n := range counts {
		if n != 1 {
			t.Fatalf("event %s %s %d times", id, what, n)
		}
	}
}

func TestDBPendingStreamsReturnsOnlySubscribedStreams(t *testing.T) {
	s, binding := dbStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	logEvent := eventFor(t, Logs, now.Add(-time.Hour))
	plan := eventFor(t, Plans, now.Add(-30*time.Minute))
	for _, e := range []Event{logEvent, plan} {
		before, err := s.LoadCursor(ctx, binding, e.Stream)
		if err != nil {
			t.Fatal(err)
		}
		err = s.CommitBatch(ctx, binding, e.Stream, before, before, []Event{e}, SourceRetention)
		if err != nil {
			t.Fatal(err)
		}
	}
	only, err := s.PendingStreams(ctx, binding, "streams.v1", []Stream{Plans}, 10, now)
	if err != nil || len(only) != 1 || only[0].ID != plan.ID {
		t.Fatalf("subscribed pending = %d %v", len(only), err)
	}
	all, err := s.PendingStreams(ctx, binding, "streams.v1", nil, 10, now)
	if err != nil || len(all) != 2 || all[0].ID != logEvent.ID {
		t.Fatalf("all-stream pending = %d %v", len(all), err)
	}
}
