package analyzer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-04: the zero-scan clock of unused_index lived only in memory, so
// a sidecar restart resolved every open zero-scan finding on its first
// cycle and reopened it as a new row a window later. The clock is now
// persisted in sage.unused_index_clock; these tests drive the analyzer's
// clock store through a fake table (unused_clock_db_test.go runs the SQL).

// No concurrent access tests here: the store is touched only by the
// analyzer's cycle goroutine; fleet isolation is tested on real databases
// in unused_clock_db_test.go.

const clockIdent = "public.ix_clock"

var clockNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

type fakeClockDB struct {
	rows        map[string]clockRow
	open        []string
	docs        []indexHistoryDoc
	loadErr     error
	openErr     error
	historyErr  error
	saveErr     error
	loads       int
	saves       int
	historyArgs []string
	lastUpserts map[string]clockRow
	lastDeletes []string
}

func (f *fakeClockDB) load(context.Context) (map[string]clockRow, error) {
	f.loads++
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	out := make(map[string]clockRow, len(f.rows))
	for k, v := range f.rows {
		out[k] = v
	}
	return out, nil
}

func (f *fakeClockDB) openZeroScan(context.Context) ([]string, error) {
	return f.open, f.openErr
}

func (f *fakeClockDB) history(_ context.Context, idents []string, _ time.Time,
	_ int) ([]indexHistoryDoc, error) {
	f.historyArgs = append(f.historyArgs, idents...)
	return f.docs, f.historyErr
}

func (f *fakeClockDB) save(_ context.Context, up map[string]clockRow, del []string) error {
	f.saves++
	f.lastUpserts, f.lastDeletes = up, del
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.rows == nil {
		f.rows = map[string]clockRow{}
	}
	for k, v := range up {
		f.rows[k] = v
	}
	for _, k := range del {
		delete(f.rows, k)
	}
	return nil
}

type clockHarness struct {
	a    *Analyzer
	db   *fakeClockDB
	now  time.Time
	logs []string
}

func newClockHarness(db *fakeClockDB) *clockHarness {
	h := &clockHarness{db: db, now: clockNow}
	h.a = New(nil, phase2Config(), nil, nil, nil, nil, nil,
		func(level, format string, args ...any) {
			h.logs = append(h.logs, level+" "+fmt.Sprintf(format, args...))
		})
	h.a.unusedClock = newUnusedClockStore(db)
	h.a.extras.Now = func() time.Time { return h.now }
	return h
}

// cycle runs the parts of one analyzer cycle that own the clock and
// returns the unused_index findings and the resolvable categories.
func (h *clockHarness) cycle(snap *collector.Snapshot) ([]Finding, map[string]bool) {
	ctx := context.Background()
	h.a.eval = newCycleEval()
	h.a.prepareUnusedClock(ctx, snap)
	var unused []Finding
	for _, f := range h.a.runSnapshotRules(snap, nil, false) {
		if f.Category == "unused_index" {
			unused = append(unused, f)
		}
	}
	res := h.a.eval.resolvable(unused)
	h.a.saveUnusedClock(ctx)
	return unused, res
}

func (h *clockHarness) kept() map[string]bool { return h.a.eval.kept("unused_index") }

func clockSnap(oid uint32, scans int64) *collector.Snapshot {
	return &collector.Snapshot{Indexes: []collector.IndexStats{{
		SchemaName: "public", RelName: "t_clock", IndexRelName: "ix_clock",
		IndexRelID: oid, IdxScan: scans, IsValid: true,
		IndexDef: "CREATE INDEX ix_clock ON public.t_clock USING btree (a)",
	}}}
}

func unusedSinceOf(t *testing.T, ff []Finding) string {
	t.Helper()
	if len(ff) != 1 {
		t.Fatalf("got %d unused_index findings, want 1: %+v", len(ff), ff)
	}
	s, _ := ff[0].Detail["unused_since"].(string)
	return s
}

// State transition: an open zero-scan finding, a new analyzer, an empty
// clock table and no history. Nothing is emitted and nothing resolves:
// the open finding is kept until its clock reaches the window.
func TestUnusedClock_RestartWithEmptyTableKeepsOpenFinding(t *testing.T) {
	h := newClockHarness(&fakeClockDB{open: []string{clockIdent}})
	got, res := h.cycle(clockSnap(100, 0))
	if len(got) != 0 {
		t.Fatalf("no persisted history must emit nothing, got %+v", got)
	}
	if res["unused_index"] && !h.kept()[clockIdent] {
		t.Fatalf("open finding would resolve: resolvable=%v kept=%v", res, h.kept())
	}
	if !h.kept()[clockIdent] {
		t.Fatalf("held identity not kept open: %v", h.kept())
	}
	row, ok := h.db.rows[clockIdent]
	if !ok || row.oid != 100 || !row.since.Equal(clockNow) {
		t.Fatalf("clock persisted as %+v (present %v), want oid 100 since now", row, ok)
	}
	if len(h.db.historyArgs) != 1 || h.db.historyArgs[0] != clockIdent {
		t.Fatalf("history read for %v, want the one open identity", h.db.historyArgs)
	}
}

// Happy path: a persisted sighting 8 days ago with the same oid gives a
// finding whose unused_since is that sighting.
func TestUnusedClock_PersistedSightingGivesFinding(t *testing.T) {
	since := clockNow.Add(-8 * day)
	db := &fakeClockDB{rows: map[string]clockRow{clockIdent: {oid: 100, since: since}},
		open: []string{clockIdent}}
	h := newClockHarness(db)
	got, res := h.cycle(clockSnap(100, 0))
	if s := unusedSinceOf(t, got); s != since.Format(time.RFC3339) {
		t.Fatalf("unused_since = %s, want %s", s, since.Format(time.RFC3339))
	}
	if !res["unused_index"] {
		t.Fatalf("unused_index must be resolvable after a clean load: %v", res)
	}
	if len(h.kept()) != 0 {
		t.Fatalf("an emitted finding needs no hold: %v", h.kept())
	}
	if db.saves != 0 {
		t.Fatalf("an unchanged clock wrote %d times, want 0", db.saves)
	}
	if len(db.historyArgs) != 0 {
		t.Fatalf("persisted clock still read history for %v", db.historyArgs)
	}
}

// Boundary: a sighting 6 days ago is short of the 7-day window.
func TestUnusedClock_SightingInsideWindowGivesNoFinding(t *testing.T) {
	since := clockNow.Add(-6 * day)
	h := newClockHarness(&fakeClockDB{rows: map[string]clockRow{
		clockIdent: {oid: 100, since: since}}})
	if got, _ := h.cycle(clockSnap(100, 0)); len(got) != 0 {
		t.Fatalf("6-day clock emitted %+v", got)
	}
	if fs := h.a.extras.FirstSeen[clockIdent]; !fs.Equal(since) {
		t.Fatalf("clock = %s, want the persisted sighting %s", fs, since)
	}
	h.now = since.Add(7 * day)
	if got, _ := h.cycle(clockSnap(100, 0)); len(got) != 1 {
		t.Fatalf("clock at the window emitted %d findings, want 1", len(got))
	}
}

// Boundary: another oid under the same name is a new object: its clock
// restarts now and the stored row is replaced.
func TestUnusedClock_DifferentOIDRestartsAtNow(t *testing.T) {
	db := &fakeClockDB{rows: map[string]clockRow{
		clockIdent: {oid: 99, since: clockNow.Add(-30 * day)}}}
	h := newClockHarness(db)
	if got, _ := h.cycle(clockSnap(100, 0)); len(got) != 0 {
		t.Fatalf("recreated index inherited the old clock: %+v", got)
	}
	if fs := h.a.extras.FirstSeen[clockIdent]; !fs.Equal(clockNow) {
		t.Fatalf("clock = %s, want now", fs)
	}
	row := db.rows[clockIdent]
	if row.oid != 100 || !row.since.Equal(clockNow) {
		t.Fatalf("stored row = %+v, want oid 100 since now", row)
	}
}

// Boundary: a statistics reset after the sighting clamps unused_since to
// the epoch; the clock is never backdated past it.
func TestUnusedClock_StatsResetClampsPersistedSighting(t *testing.T) {
	since := clockNow.Add(-20 * day)
	h := newClockHarness(&fakeClockDB{rows: map[string]clockRow{
		clockIdent: {oid: 100, since: since}}})
	epoch := clockNow.Add(-7*day - time.Hour)
	h.a.extras.StatsEpoch = epoch
	got, _ := h.cycle(clockSnap(100, 0))
	if s := unusedSinceOf(t, got); s != epoch.Format(time.RFC3339) {
		t.Fatalf("unused_since = %s, want the epoch %s", s, epoch.Format(time.RFC3339))
	}
	h2 := newClockHarness(&fakeClockDB{rows: map[string]clockRow{
		clockIdent: {oid: 100, since: since}}, open: []string{clockIdent}})
	h2.a.extras.StatsEpoch = clockNow.Add(-2 * day)
	got, res := h2.cycle(clockSnap(100, 0))
	if len(got) != 0 || !res["unused_index"] || h2.kept()[clockIdent] {
		t.Fatalf("a reset 2 days ago must resolve the finding: got %+v res %v kept %v",
			got, res, h2.kept())
	}
}

// Error propagation: a failed load fails the category (no resolution)
// and is retried on the next cycle.
func TestUnusedClock_LoadFailureFailsCategoryAndRetries(t *testing.T) {
	db := &fakeClockDB{loadErr: errors.New("relation \"sage.unused_index_clock\" does not exist"),
		open: []string{clockIdent}}
	h := newClockHarness(db)
	got, res := h.cycle(clockSnap(100, 0))
	if res["unused_index"] || len(got) != 0 {
		t.Fatalf("failed load: resolvable=%v findings=%+v, want neither", res, got)
	}
	if db.saves != 0 {
		t.Fatalf("an unloaded store wrote %d times, want 0", db.saves)
	}
	if !hasLog(h.logs, "WARN", "unused index clock") {
		t.Fatalf("load failure not logged: %v", h.logs)
	}
	db.loadErr = nil
	db.rows = map[string]clockRow{clockIdent: {oid: 100, since: clockNow.Add(-8 * day)}}
	got, res = h.cycle(clockSnap(100, 0))
	if db.loads != 2 || !res["unused_index"] || len(got) != 1 {
		t.Fatalf("retry: loads=%d resolvable=%v findings=%d, want 2/true/1",
			db.loads, res["unused_index"], len(got))
	}
}

// Error propagation: open findings or history that cannot be read keep
// the store unloaded, like a failed load.
func TestUnusedClock_OpenOrHistoryFailureFailsCategory(t *testing.T) {
	for name, db := range map[string]*fakeClockDB{
		"open":    {openErr: errors.New("conn closed")},
		"history": {open: []string{clockIdent}, historyErr: errors.New("timeout")},
	} {
		h := newClockHarness(db)
		if _, res := h.cycle(clockSnap(100, 0)); res["unused_index"] {
			t.Fatalf("%s failure left unused_index resolvable", name)
		}
		if db.saves != 0 {
			t.Fatalf("%s failure wrote the clock", name)
		}
	}
}

// Error propagation: a failed save logs an error with the ident count,
// keeps the in-memory clock and is retried.
func TestUnusedClock_SaveFailureKeepsMemoryAndRetries(t *testing.T) {
	db := &fakeClockDB{saveErr: errors.New("disk full")}
	h := newClockHarness(db)
	h.cycle(clockSnap(100, 0))
	if !hasLog(h.logs, "ERROR", "1 idents changed") {
		t.Fatalf("save failure not logged with the ident count: %v", h.logs)
	}
	if fs := h.a.extras.FirstSeen[clockIdent]; !fs.Equal(clockNow) {
		t.Fatalf("in-memory clock lost after a failed save: %s", fs)
	}
	db.saveErr = nil
	h.now = clockNow.Add(time.Minute)
	h.cycle(clockSnap(100, 0))
	if db.saves != 2 || !db.rows[clockIdent].since.Equal(clockNow) {
		t.Fatalf("retry: saves=%d row=%+v, want 2 and the first sighting",
			db.saves, db.rows[clockIdent])
	}
}

func hasLog(logs []string, level, fragment string) bool {
	for _, l := range logs {
		if strings.HasPrefix(l, level+" ") && strings.Contains(l, fragment) {
			return true
		}
	}
	return false
}

// Nil/empty: no open findings and an empty table end the warmup on the
// first cycle.
func TestUnusedClock_EmptyTableNoOpenFindingsReadyAtOnce(t *testing.T) {
	db := &fakeClockDB{}
	h := newClockHarness(db)
	_, res := h.cycle(clockSnap(100, 0))
	if !res["unused_index"] || len(h.kept()) != 0 {
		t.Fatalf("resolvable=%v kept=%v, want resolvable and nothing kept", res, h.kept())
	}
	if db.saves != 1 || db.rows[clockIdent].oid != 100 {
		t.Fatalf("first sighting not persisted: saves=%d rows=%+v", db.saves, db.rows)
	}
	if len(db.historyArgs) != 0 {
		t.Fatalf("history read with no open finding: %v", db.historyArgs)
	}
}

// Nil: an analyzer without a database keeps the in-memory clock only.
func TestUnusedClock_NoDatabaseIsReady(t *testing.T) {
	h := newClockHarness(nil)
	h.a.unusedClock = newUnusedClockStore(nil)
	_, res := h.cycle(clockSnap(100, 0))
	if !res["unused_index"] {
		t.Fatalf("no database: unused_index not resolvable: %v", res)
	}
	if fs := h.a.extras.FirstSeen[clockIdent]; !fs.Equal(clockNow) {
		t.Fatalf("in-memory clock = %s, want now", fs)
	}
}

// Invalid input: missing index list keeps the store unloaded (a load
// would forget every row), and a cycle whose stats epoch failed does not
// load either: the epoch decides which findings are held.
func TestUnusedClock_NoLoadWithoutIndexesOrEpoch(t *testing.T) {
	db := &fakeClockDB{}
	h := newClockHarness(db)
	empty := &collector.Snapshot{Unavailable: map[string]string{"indexes": "timeout"}}
	if _, res := h.cycle(empty); res["unused_index"] || db.loads != 0 {
		t.Fatalf("unavailable indexes: resolvable=%v loads=%d", res, db.loads)
	}
	h.a.eval = newCycleEval()
	h.a.eval.fail("unused_index") // loadStatsEpoch failed
	h.a.prepareUnusedClock(context.Background(), clockSnap(100, 0))
	if db.loads != 0 {
		t.Fatalf("store loaded in a cycle without a stats epoch")
	}
}
