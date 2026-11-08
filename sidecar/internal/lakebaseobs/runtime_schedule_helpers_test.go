package lakebaseobs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// scheduleEpoch is the fake clock's first poll; background rows start
// eight days earlier so every window in the source horizon holds rows.
var scheduleEpoch = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
var rowBase = scheduleEpoch.Add(-8 * 24 * time.Hour)

// memCursors is an in-memory cursorStore with the same revision CAS as the
// durable store. It records which pass produced each commit.
type memCursors struct {
	mu        sync.Mutex
	cursors   map[Stream]Cursor
	seen      map[string]bool
	firstPass map[string]pass
	commits   []commitRecord
}

type commitRecord struct {
	stream Stream
	pass   pass
	events int
}

func newMemCursors() *memCursors {
	return &memCursors{cursors: map[Stream]Cursor{}, seen: map[string]bool{},
		firstPass: map[string]pass{}}
}

func (m *memCursors) LoadCursor(_ context.Context, _ string, s Stream) (Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursors[s], nil
}

func (m *memCursors) CommitBatch(_ context.Context, _ string, s Stream, before, after Cursor,
	events []Event, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cursors[s].Revision != before.Revision {
		return ErrConflict
	}
	p := commitPass(before, after)
	after.Revision = before.Revision + 1
	m.cursors[s] = after
	for _, e := range events {
		if !m.seen[e.ID] {
			m.firstPass[e.ID] = p
		}
		m.seen[e.ID] = true
	}
	m.commits = append(m.commits, commitRecord{s, p, len(events)})
	return nil
}

func (m *memCursors) cursor(s Stream) Cursor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursors[s]
}

// commitPass classifies a commit by the cursor edge it moved.
func commitPass(before, after Cursor) pass {
	switch {
	case !after.ForwardThrough.Equal(before.ForwardThrough):
		return forwardPass
	case !after.ShortThrough.Equal(before.ShortThrough):
		return shortPass
	default:
		return deepPass
	}
}

type lateRow struct {
	stream      Stream
	at, visible time.Time
	id          string
}

type readRecord struct {
	stream Stream
	w      Window
	err    error
}

// rateReader emits one background row per stream every `every`, plus late
// rows that only become visible at their visibility time on the fake clock.
type rateReader struct {
	mu      sync.Mutex
	hash    string
	every   time.Duration
	maxRows int
	now     time.Time
	late    []lateRow
	fail    map[Stream]error
	reads   []readRecord
}

func rowIndex(t time.Time, every time.Duration) int64 {
	d := t.Sub(rowBase)
	k := int64(d / every)
	if d%every != 0 {
		k++
	}
	return k
}

func (f *rateReader) event(s Stream, at time.Time, id string) Event {
	return Event{ID: id, Stream: s, ScopeHash: f.hash, ComputeID: "compute-1", OccurredAt: at,
		ReceivedAt: f.now, Version: SchemaVersion}
}

func (f *rateReader) ReadWindow(_ context.Context, s Stream, _ Scope, w Window) (Batch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := f.read(s, w)
	f.reads = append(f.reads, readRecord{s, w, err})
	return b, err
}

func (f *rateReader) read(s Stream, w Window) (Batch, error) {
	if err := f.fail[s]; err != nil {
		return Batch{}, err
	}
	var events []Event
	if f.every > 0 {
		first, last := rowIndex(w.From, f.every), rowIndex(w.To, f.every)
		if visible := rowIndex(f.now, f.every); visible < last {
			last = visible
		}
		if last-first > int64(f.maxRows) {
			return Batch{}, ErrBudget
		}
		for k := first; k < last; k++ {
			at := rowBase.Add(time.Duration(k) * f.every)
			events = append(events, f.event(s, at, fmt.Sprintf("%s-%d", s, k)))
		}
	}
	for _, r := range f.late {
		if r.stream == s && !r.visible.After(f.now) && !r.at.Before(w.From) && r.at.Before(w.To) {
			events = append(events, f.event(s, r.at, r.id))
		}
	}
	if len(events) > f.maxRows {
		return Batch{}, ErrBudget
	}
	return Batch{Events: events}, nil
}

func (f *rateReader) readsSince(n int) []readRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]readRecord(nil), f.reads[n:]...)
}

func (f *rateReader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

func scheduleRuntime(t *testing.T, f *rateReader, mutate func(*Options)) *Runtime {
	t.Helper()
	o := Options{WarehouseID: "test"}
	if mutate != nil {
		mutate(&o)
	}
	rt, err := NewRuntime(f, &Store{}, testScope(), o)
	if err != nil {
		t.Fatal(err)
	}
	f.hash = rt.scope.Hash()
	if f.maxRows == 0 {
		f.maxRows = rt.options.MaxRows
	}
	return rt
}

// pollAt runs one poll's collection against the fake clock.
func pollAt(rt *Runtime, mem *memCursors, f *rateReader, now time.Time) []error {
	f.mu.Lock()
	f.now = now
	f.mu.Unlock()
	return rt.collect(context.Background(), mem, now)
}
