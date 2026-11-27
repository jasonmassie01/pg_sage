package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-04: transitions of a held finding (an open zero-scan finding whose
// clock has not yet reached the window after a restart) and the one-time
// history backfill for open rows that predate sage.unused_index_clock.

// No concurrent access tests: see unused_clock_test.go.

func zeroDoc(at time.Time, oid uint32, scans int64) indexHistoryDoc {
	return indexHistoryDoc{at: at, indexes: map[string]historyIndex{
		clockIdent: {oid: oid, scans: scans}}}
}

// State transition: held -> emitted. The kept finding is re-emitted (same
// identity) once the clock reaches the window, and the hold ends.
func TestUnusedClock_HeldFindingReemittedAtWindow(t *testing.T) {
	h := newClockHarness(&fakeClockDB{open: []string{clockIdent}})
	h.cycle(clockSnap(100, 0))
	for _, at := range []time.Duration{time.Hour, 3 * day, 7*day - time.Minute} {
		h.now = clockNow.Add(at)
		got, _ := h.cycle(clockSnap(100, 0))
		if len(got) != 0 || !h.kept()[clockIdent] {
			t.Fatalf("now+%s: findings=%d kept=%v, want held", at, len(got), h.kept())
		}
	}
	h.now = clockNow.Add(7 * day)
	got, _ := h.cycle(clockSnap(100, 0))
	if s := unusedSinceOf(t, got); s != clockNow.Format(time.RFC3339) {
		t.Fatalf("unused_since = %s, want the warmup sighting", s)
	}
	if len(h.kept()) != 0 || len(h.a.unusedClock.held) != 0 {
		t.Fatalf("hold survived the emitted finding: kept=%v held=%v",
			h.kept(), h.a.unusedClock.held)
	}
}

// State transition: held -> resolved by evidence. A scan, a dropped index,
// a recreated index (new oid), a counter decrease and a later reset each
// end the hold, so the finding resolves as before rules-04.
func TestUnusedClock_HoldEndsOnPositiveEvidence(t *testing.T) {
	scanned := clockSnap(100, 5)
	recreated := clockSnap(101, 0)
	gone := &collector.Snapshot{Indexes: []collector.IndexStats{{
		SchemaName: "public", RelName: "t_clock", IndexRelName: "ix_other",
		IndexRelID: 7, IsValid: true, IsUnique: true}}}
	for name, snap := range map[string]*collector.Snapshot{
		"scanned": scanned, "recreated": recreated, "dropped": gone,
	} {
		h := newClockHarness(&fakeClockDB{open: []string{clockIdent}})
		h.cycle(clockSnap(100, 0))
		h.now = clockNow.Add(time.Hour)
		_, res := h.cycle(snap)
		if !res["unused_index"] || h.kept()[clockIdent] {
			t.Fatalf("%s: resolvable=%v kept=%v, want the finding to resolve",
				name, res["unused_index"], h.kept())
		}
		if len(h.a.unusedClock.held) != 0 {
			t.Fatalf("%s: hold not ended: %v", name, h.a.unusedClock.held)
		}
	}
	h := newClockHarness(&fakeClockDB{open: []string{clockIdent}})
	h.cycle(clockSnap(100, 0))
	h.now = clockNow.Add(time.Hour)
	h.a.extras.StatsEpoch = clockNow.Add(30 * time.Minute)
	if _, res := h.cycle(clockSnap(100, 0)); !res["unused_index"] || h.kept()[clockIdent] {
		t.Fatalf("reset after the hold began: kept=%v, want resolved", h.kept())
	}
}

// A counter that went down restarts the clock; it is a reset the epoch
// does not show, so the held finding resolves.
func TestUnusedClock_HoldEndsOnCounterDecrease(t *testing.T) {
	h := newClockHarness(&fakeClockDB{open: []string{clockIdent}})
	h.cycle(clockSnap(100, 0))
	h.now = clockNow.Add(time.Hour)
	ctx := t.Context()
	h.a.eval = newCycleEval()
	h.a.prepareUnusedClock(ctx, clockSnap(100, 0))
	prev := clockSnap(100, 3) // the previous sample saw scans (PG14, reset since)
	h.a.runSnapshotRules(clockSnap(100, 0), prev, false)
	if h.kept()[clockIdent] {
		t.Fatalf("decrease kept the held finding open")
	}
}

// The stats epoch failing in a later cycle neither ends nor uses a hold:
// the category is failed for that cycle and the hold is kept for the next.
func TestUnusedClock_HoldSurvivesFailedEpochCycle(t *testing.T) {
	h := newClockHarness(&fakeClockDB{open: []string{clockIdent}})
	h.cycle(clockSnap(100, 0))
	h.now = clockNow.Add(time.Hour)
	h.a.eval = newCycleEval()
	h.a.extras.StatsEpoch = h.now // what loadStatsEpoch sets on failure
	h.a.eval.fail("unused_index")
	h.a.prepareUnusedClock(t.Context(), clockSnap(100, 0))
	h.a.runSnapshotRules(clockSnap(100, 0), nil, false)
	if len(h.a.unusedClock.held) != 1 {
		t.Fatalf("failed-epoch cycle ended the hold: %v", h.a.unusedClock.held)
	}
	h.a.extras.StatsEpoch = time.Time{}
	h.now = clockNow.Add(2 * time.Hour)
	if _, _ = h.cycle(clockSnap(100, 0)); !h.kept()[clockIdent] {
		t.Fatalf("hold lost after the failed-epoch cycle")
	}
}

// Backfill: eight daily history documents at zero scans with the same oid
// give the earliest one as the clock, so the open finding is re-emitted
// on the first cycle after the upgrade.
func TestUnusedClock_BackfillFromContinuousHistory(t *testing.T) {
	var docs []indexHistoryDoc
	for d := 0; d <= 7; d++ {
		docs = append(docs, zeroDoc(clockNow.Add(-time.Duration(d)*day-time.Hour), 100, 0))
	}
	db := &fakeClockDB{open: []string{clockIdent}, docs: docs}
	h := newClockHarness(db)
	got, _ := h.cycle(clockSnap(100, 0))
	oldest := clockNow.Add(-7*day - time.Hour)
	if s := unusedSinceOf(t, got); s != oldest.Format(time.RFC3339) {
		t.Fatalf("unused_since = %s, want the oldest zero-scan day %s", s,
			oldest.Format(time.RFC3339))
	}
	if !db.rows[clockIdent].since.Equal(oldest) {
		t.Fatalf("backfilled clock not persisted: %+v", db.rows[clockIdent])
	}
}

// Backfill boundary: continuity stops at a day with scans, another oid or
// no entry; a shorter clock holds the finding instead of resolving it.
func TestUnusedClock_BackfillStopsAtBrokenContinuity(t *testing.T) {
	d1, d2, d3 := clockNow.Add(-day), clockNow.Add(-2*day), clockNow.Add(-3*day)
	for name, third := range map[string]indexHistoryDoc{
		"scans":   zeroDoc(d3, 100, 4),
		"oid":     zeroDoc(d3, 55, 0),
		"missing": {at: d3, indexes: map[string]historyIndex{}},
	} {
		docs := []indexHistoryDoc{zeroDoc(d1, 100, 0), zeroDoc(d2, 100, 0), third,
			zeroDoc(clockNow.Add(-4*day), 100, 0)}
		got, ok := earliestZeroScan(docs, clockIdent, 100)
		if !ok || !got.Equal(d2) {
			t.Fatalf("%s: earliest = %s (%v), want %s", name, got, ok, d2)
		}
	}
	if _, ok := earliestZeroScan([]indexHistoryDoc{zeroDoc(d1, 100, 1)}, clockIdent, 100); ok {
		t.Fatalf("a newest day with scans must give no sighting")
	}
	if _, ok := earliestZeroScan(nil, clockIdent, 100); ok {
		t.Fatalf("no history must give no sighting")
	}
	db := &fakeClockDB{open: []string{clockIdent},
		docs: []indexHistoryDoc{zeroDoc(d1, 100, 0), zeroDoc(d2, 100, 0)}}
	h := newClockHarness(db)
	if got, _ := h.cycle(clockSnap(100, 0)); len(got) != 0 || !h.kept()[clockIdent] {
		t.Fatalf("2-day history: findings=%d kept=%v, want held", len(got), h.kept())
	}
	if !db.rows[clockIdent].since.Equal(d2) {
		t.Fatalf("clock = %+v, want the 2-day sighting", db.rows[clockIdent])
	}
}

// Invalid input: an unknown oid (0) is never persisted or matched, and an
// open finding whose index is gone or scanned is not backfilled or held.
func TestUnusedClock_UnknownOIDAndStaleOpenRows(t *testing.T) {
	db := &fakeClockDB{rows: map[string]clockRow{
		clockIdent: {oid: 0, since: clockNow.Add(-30 * day)}}}
	h := newClockHarness(db)
	if got, _ := h.cycle(clockSnap(0, 0)); len(got) != 0 {
		t.Fatalf("oid-0 row was used as evidence: %+v", got)
	}
	if _, ok := db.rows[clockIdent]; ok {
		t.Fatalf("oid-0 clock persisted: %+v", db.rows)
	}
	db2 := &fakeClockDB{open: []string{clockIdent, "public.ix_gone"}}
	h2 := newClockHarness(db2)
	if _, res := h2.cycle(clockSnap(100, 5)); !res["unused_index"] || len(h2.kept()) != 0 {
		t.Fatalf("scanned/gone open rows: resolvable=%v kept=%v", res, h2.kept())
	}
	if len(db2.historyArgs) != 0 {
		t.Fatalf("history read for scanned or gone indexes: %v", db2.historyArgs)
	}
}

// Boundary: the store writes only what changed: nothing on a steady
// cycle, the forgotten row on a scan.
func TestUnusedClock_SavesOnlyChanges(t *testing.T) {
	db := &fakeClockDB{}
	h := newClockHarness(db)
	h.cycle(clockSnap(100, 0))
	h.now = clockNow.Add(time.Minute)
	h.cycle(clockSnap(100, 0))
	if db.saves != 1 {
		t.Fatalf("steady cycle wrote: saves=%d, want 1", db.saves)
	}
	h.now = clockNow.Add(2 * time.Minute)
	h.cycle(clockSnap(100, 2))
	if db.saves != 2 || len(db.lastUpserts) != 0 || len(db.lastDeletes) != 1 ||
		db.lastDeletes[0] != clockIdent {
		t.Fatalf("scan: saves=%d upserts=%v deletes=%v, want one delete of %s",
			db.saves, db.lastUpserts, db.lastDeletes, clockIdent)
	}
}
