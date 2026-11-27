package lakebaseobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// spanReader refuses windows wider than limit and returns fixed events.
type spanReader struct {
	mu     sync.Mutex
	limit  time.Duration
	events []Event
	spans  []time.Duration
	hits   map[string]int
}

func (r *spanReader) ReadWindow(_ context.Context, s Stream, _ Scope, w Window) (Batch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, w.To.Sub(w.From))
	if w.To.Sub(w.From) > r.limit {
		return Batch{}, ErrBudget
	}
	var out []Event
	for _, e := range r.events {
		if e.Stream == s && !e.OccurredAt.Before(w.From) && e.OccurredAt.Before(w.To) {
			out = append(out, e)
			r.hits[e.ID]++
		}
	}
	return Batch{Events: out}, nil
}

func (r *spanReader) take() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.spans
	r.spans = nil
	return out
}

func planEvent(t *testing.T, scope Scope, at time.Time) Event {
	t.Helper()
	e, err := NewEvent(Plans, scope, testFields(Plans, at), at.Add(time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDBRuntimeNarrowedWidthSurvivesRestartAndGrowsBack(t *testing.T) {
	ctx := context.Background()
	reader := &spanReader{limit: 2 * time.Minute, hits: map[string]int{}}
	o := Options{WarehouseID: "test", AllowPayload: true, Streams: []Stream{Plans},
		QueriesPerPoll: 4}
	rt, s := runtimeStore(t, reader, o)
	now := time.Now().UTC().Truncate(time.Second)
	if err := rt.Poll(ctx, now); err != nil {
		t.Fatal(err)
	}
	c, err := s.LoadCursor(ctx, rt.Binding(), Plans)
	if want := 15 * time.Minute / 8; err != nil || c.Width != want || c.Revision != 1 {
		t.Fatalf("persisted width %v revision %d err %v, want %v", c.Width, c.Revision, err, want)
	}
	restarted, err := NewRuntime(reader, s, rt.Scope(), o)
	if err != nil || restarted.Binding() != rt.Binding() {
		t.Fatalf("restart binding %v err %v", restarted, err)
	}
	reader.take()
	if err = restarted.Poll(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if spans := reader.take(); len(spans) == 0 || spans[0] != c.Width {
		t.Fatalf("restarted runtime first read %v, want persisted width %v", spans, c.Width)
	}
	reader.mu.Lock()
	reader.limit = SourceRetention
	reader.mu.Unlock()
	for i := 2; i < 40; i++ {
		if err = restarted.Poll(ctx, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	c, err = s.LoadCursor(ctx, rt.Binding(), Plans)
	if err != nil || c.Width != 15*time.Minute {
		t.Fatalf("width %v after sparse reads, want Window; err %v", c.Width, err)
	}
	st := restarted.Status()[0]
	if st.ForwardLagSeconds == nil || *st.ForwardLagSeconds > 180 || *st.ForwardLagSeconds < 120 {
		t.Fatalf("status forward lag %v", st.ForwardLagSeconds)
	}
}

func TestDBRuntimeOverlappingPassesKeepOneInboxRow(t *testing.T) {
	ctx := context.Background()
	reader := &spanReader{limit: SourceRetention, hits: map[string]int{}}
	// A seven-day window lets forward, short and deep each read every event.
	rt, s := runtimeStore(t, reader, Options{WarehouseID: "test", AllowPayload: true,
		Streams: []Stream{Plans}, Window: SourceRetention})
	now := time.Now().UTC().Truncate(time.Second)
	for _, age := range []time.Duration{90, 60, 30, 10, 5} {
		reader.events = append(reader.events, planEvent(t, rt.Scope(), now.Add(-age*time.Minute)))
	}
	for i := 0; i < 3; i++ {
		if err := rt.Poll(ctx, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range reader.events {
		if reader.hits[e.ID] < 2 {
			t.Fatalf("event %s read %d times; passes did not overlap", e.ID, reader.hits[e.ID])
		}
	}
	var rows int
	err := s.pool.QueryRow(ctx, "SELECT count(*) FROM sage.lakebaseobs_inbox WHERE binding=$1",
		rt.Binding()).Scan(&rows)
	if err != nil || rows != len(reader.events) {
		t.Fatalf("inbox rows %d err %v, want %d distinct", rows, err, len(reader.events))
	}
}

// racingCursors commits a competing cursor write before the runtime's first commit.
type racingCursors struct {
	*Store
	raced bool
}

func (r *racingCursors) CommitBatch(ctx context.Context, binding string, s Stream,
	before, after Cursor, events []Event, retention time.Duration) error {
	if !r.raced {
		r.raced = true
		rival := before
		rival.LastGap = "rival writer"
		if err := r.Store.CommitBatch(ctx, binding, s, before, rival, nil, retention); err != nil {
			return err
		}
	}
	return r.Store.CommitBatch(ctx, binding, s, before, after, events, retention)
}

func TestDBRuntimeCursorCASConflictIsStillDetected(t *testing.T) {
	ctx := context.Background()
	reader := &spanReader{limit: SourceRetention, hits: map[string]int{}}
	rt, s := runtimeStore(t, reader, Options{WarehouseID: "test", AllowPayload: true,
		Streams: []Stream{Plans}})
	now := time.Now().UTC().Truncate(time.Second)
	reader.events = []Event{planEvent(t, rt.Scope(), now.Add(-5*time.Minute))}
	if err := s.BindOwner(ctx, rt.Scope()); err != nil {
		t.Fatal(err)
	}
	if err := s.checkPolicy(ctx, rt.Binding(), true); err != nil {
		t.Fatal(err)
	}
	errs := rt.collect(ctx, &racingCursors{Store: s}, now)
	if len(errs) != 1 || !errors.Is(errs[0], ErrConflict) {
		t.Fatalf("errors %v, want cursor conflict", errs)
	}
	c, err := s.LoadCursor(ctx, rt.Binding(), Plans)
	if err != nil || c.Revision != 1 || c.LastGap != "rival writer" || !c.ForwardThrough.IsZero() {
		t.Fatalf("rival cursor overwritten: %+v err %v", c, err)
	}
	var rows int
	if err = s.pool.QueryRow(ctx, "SELECT count(*) FROM sage.lakebaseobs_inbox WHERE binding=$1",
		rt.Binding()).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rejected batch left %d inbox rows, err %v", rows, err)
	}
	if st := rt.Status()[0]; st.LastError != ErrConflict.Error() {
		t.Fatalf("status error %q", st.LastError)
	}
}
