package snapstore

import (
	"strings"
	"testing"
	"time"
)

// sage.snapshots is partitioned by UTC day and retention drops whole days.
// A delta must never name a base in an earlier day, or dropping that day
// would orphan it: the first document of a new UTC day is a keyframe even
// when the previous keyframe is younger than keyframeMaxAge.
func TestPlan_NewUTCDayStartsANewKeyframe(t *testing.T) {
	w := NewWriter()
	doc := list(idx("a", 1), idx("b", 2))
	late := time.Date(2026, 10, 2, 23, 58, 0, 0, time.UTC)
	withBase(t, w, "indexes", 9, late, doc)
	p, err := w.plan("indexes", list(idx("a", 1), idx("b", 3)), late.Add(time.Minute))
	if err != nil || p.baseID != 9 {
		t.Fatalf("23:59: plan = %+v (%v), want a delta on the same day", p, err)
	}
	p, err = w.plan("indexes", list(idx("a", 1), idx("b", 4)), late.Add(2*time.Minute))
	if err != nil || p.baseID != 0 || p.next == nil {
		t.Fatalf("00:00 next day: plan = %+v (%v), want a keyframe", p, err)
	}
}

// The day is the UTC day whatever zone the collector's clock is in.
func TestPlan_DayBoundaryIsUTC(t *testing.T) {
	w := NewWriter()
	doc := list(idx("a", 1))
	east := time.FixedZone("UTC+10", 10*3600)
	// 09:00 and 09:30 on the 3rd at UTC+10 are 23:00 and 23:30 on the 2nd UTC.
	withBase(t, w, "indexes", 3, time.Date(2026, 10, 3, 9, 0, 0, 0, east), doc)
	p, err := w.plan("indexes", list(idx("a", 2)), time.Date(2026, 10, 3, 9, 30, 0, 0, east))
	if err != nil || p.baseID != 3 {
		t.Fatalf("same UTC day in another zone: plan = %+v (%v), want a delta", p, err)
	}
	p, err = w.plan("indexes", list(idx("a", 3)), time.Date(2026, 10, 3, 10, 0, 0, 0, east))
	if err != nil || p.baseID != 0 {
		t.Fatalf("next UTC day: plan = %+v (%v), want a keyframe", p, err)
	}
}

// A checkpoint from the previous UTC day is not a base either.
func TestPlan_CheckpointFromPreviousDayIsNotUsed(t *testing.T) {
	w := NewWriter()
	late := time.Date(2026, 10, 2, 23, 0, 0, 0, time.UTC)
	withBase(t, w, "indexes", 1, late, indexList(40, nil))
	cp, err := w.plan("indexes", indexList(40, map[int]int{0: 5, 1: 5}), late.Add(time.Minute))
	if err != nil || cp.checkpoint == nil {
		t.Fatalf("checkpoint plan = %+v (%v)", cp, err)
	}
	commit(w, "indexes", cp, 2)
	next := late.Add(61 * time.Minute)
	p, err := w.plan("indexes", indexList(40, map[int]int{0: 6, 1: 5}), next)
	if err != nil || p.baseID != 0 || p.next == nil {
		t.Fatalf("next day after a checkpoint: plan = %+v (%v), want a keyframe", p, err)
	}
}

// Readers pass the row's collection time so the base lookup is confined to
// the row's day partition.
func TestDataSQLPassesCollectionTime(t *testing.T) {
	if got := DataSQL("s"); got != "sage.snapshot_data(s.data, s.base_id, s.collected_at)" {
		t.Fatalf("DataSQL = %q", got)
	}
	if got := DataSQL(""); got != "sage.snapshot_data(data, base_id, collected_at)" {
		t.Fatalf("DataSQL(\"\") = %q", got)
	}
	if strings.Contains(DataSQL("x"), "x.x.") {
		t.Fatal("alias doubled")
	}
}
