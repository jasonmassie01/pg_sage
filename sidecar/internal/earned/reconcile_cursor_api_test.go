package earned

import (
	"context"
	"strings"
	"testing"
	"time"
)

// earned-02 unit and store tests of the incident cursors' own API.

func ids64(from, to int64) []int64 {
	var out []int64
	for id := from; id <= to; id++ {
		out = append(out, id)
	}
	return out
}

func rowsOf(ids ...int64) []executed {
	out := make([]executed, 0, len(ids))
	for _, id := range ids {
		out = append(out, executed{actionLogID: id})
	}
	return out
}

func ptr64(v int64) *int64 { return &v }

func TestNextIncidentCursorFirstPassInsideTheLagKeepsNothing(t *testing.T) {
	next, dropped := nextIncidentCursor(incidentCursor{}, nil, rowsOf(4, 7),
		map[int64]bool{4: true, 7: true})
	if next.cursor != nil || len(next.pending) != 0 || len(dropped) != 0 {
		t.Fatalf("next = %+v, dropped %v: rows above the cursor are read forward again",
			next, dropped)
	}
}

func TestNextIncidentCursorKeepsUndoneIdsAtOrBelowTheCursor(t *testing.T) {
	cur := incidentCursor{cursor: ptr64(10), pending: []int64{5, 6}}
	undone := map[int64]bool{5: true, 15: true, 20: true, 25: true}
	next, dropped := nextIncidentCursor(cur, ptr64(20), rowsOf(5, 6, 12, 15, 20, 25), undone)
	requireCursor(t, "next", next.cursor, 20)
	requirePending(t, next.pending, 5, 15, 20)
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v, want none", dropped)
	}
}

func TestNextIncidentCursorNeverMovesBack(t *testing.T) {
	next, _ := nextIncidentCursor(incidentCursor{cursor: ptr64(50)}, ptr64(40), rowsOf(40),
		map[int64]bool{40: true})
	requireCursor(t, "next", next.cursor, 50)
	requirePending(t, next.pending, 40)
	next, _ = nextIncidentCursor(incidentCursor{cursor: ptr64(50)}, nil, nil, nil)
	requireCursor(t, "unchanged", next.cursor, 50)
}

func TestNextIncidentCursorCapsPendingDroppingTheOldest(t *testing.T) {
	undone := map[int64]bool{}
	for _, id := range ids64(1, incidentPendingCap+100) {
		undone[id] = true
	}
	next, dropped := nextIncidentCursor(incidentCursor{},
		ptr64(int64(incidentPendingCap+100)), rowsOf(ids64(1, incidentPendingCap+100)...),
		undone)
	if len(next.pending) != incidentPendingCap || next.pending[0] != 101 ||
		len(dropped) != 100 || dropped[0] != 1 || dropped[99] != 100 {
		t.Fatalf("pending %d from %v, dropped %d (%v..%v): want the 100 oldest dropped",
			len(next.pending), next.pending[:1], len(dropped), dropped[:1],
			dropped[len(dropped)-1:])
	}
}

func TestIncidentCursorsEqual(t *testing.T) {
	a := incidentCursors{auto: incidentCursor{cursor: ptr64(3), pending: []int64{1}}}
	b := incidentCursors{auto: incidentCursor{cursor: ptr64(3), pending: []int64{1}}}
	if !a.equal(b) || a.equal(incidentCursors{}) || !(incidentCursors{}).equal(
		incidentCursors{handoff: incidentCursor{pending: []int64{}}}) {
		t.Fatal("equal must compare values, with empty and nil pending the same")
	}
	b.auto.cursor = ptr64(4)
	if a.equal(b) {
		t.Fatal("different cursors compared equal")
	}
}

// A pending id older than the lookback leaves the set and is counted.
func TestReconcileCursorDropsAPendingRowPastTheLookback(t *testing.T) {
	f := newSelfFixture(t)
	waiting := f.l3Aged("success", 10*time.Minute)
	f.attachVerification(waiting, "pending")
	last := f.l3Aged("failed", 9*time.Minute)
	r := NewReconciler(f.svc, f.pool, f.db, nil)
	runPass(t, f.ctx, r, 1)
	requirePending(t, f.incidentState().autoPending, waiting)
	f.age(waiting, 31*24*time.Hour)
	if res := runPass(t, f.ctx, r, 2); res.Dropped != 1 || res.Pending != 0 {
		t.Fatalf("pass 2 = %+v, want the aged-out pending row dropped", res)
	}
	st := f.incidentState()
	requireCursor(t, "auto", st.autoCursor, last)
	requirePending(t, st.autoPending)
}

// Concurrent writers: a save computed from a state another writer has
// since replaced is not applied (compare and set).
func TestSaveIncidentCursorsComparesAndSets(t *testing.T) {
	f := newSelfFixture(t)
	first := incidentCursors{auto: incidentCursor{cursor: ptr64(9), pending: []int64{4}}}
	second := incidentCursors{auto: incidentCursor{cursor: ptr64(7)}}
	if err := f.store.saveIncidentCursors(f.ctx, incidentCursors{}, first); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := f.store.saveIncidentCursors(f.ctx, incidentCursors{}, second); err != nil {
		t.Fatalf("stale save: %v", err)
	}
	got, err := f.store.incidentCursors(f.ctx)
	if err != nil || !got.equal(first) {
		t.Fatalf("state = %+v (%v), want the first writer's %+v", got, err, first)
	}
	next := incidentCursors{auto: incidentCursor{cursor: ptr64(12)},
		handoff: incidentCursor{cursor: ptr64(2), pending: []int64{1}}}
	if err := f.store.saveIncidentCursors(f.ctx, got, next); err != nil {
		t.Fatalf("save from the current state: %v", err)
	}
	if got, err = f.store.incidentCursors(f.ctx); err != nil || !got.equal(next) {
		t.Fatalf("state = %+v (%v), want %+v", got, err, next)
	}
}

// Error propagation: a failed read of the state names the cursors.
func TestReconcileCursorReadFailureIsNamed(t *testing.T) {
	f := newSelfFixture(t)
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.incidentCursors(canceled); err == nil ||
		!strings.Contains(err.Error(), "incident reconcile cursors") {
		t.Fatalf("read on a canceled context = %v, want an error naming the cursors", err)
	}
}
