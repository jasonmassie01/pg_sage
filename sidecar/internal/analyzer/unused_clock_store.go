package analyzer

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// The unused_index zero-scan clock (RuleExtras.FirstSeen and IndexOID) is
// persisted in sage.unused_index_clock (rules-04). Without it a sidecar
// restart started every clock again: the first cycle resolved every open
// zero-scan finding and a window later reopened it as a new row, losing
// its history, case links and occurrence count. The store loads the table
// once (retrying each cycle until it succeeds), seeds the in-memory clock
// from rows whose oid still matches, and at the end of each cycle writes
// what changed in one statement. Only the analyzer's cycle goroutine
// touches it.

// clockRow is one zero-scan clock: the index object and when it was first
// seen at zero scans.
type clockRow struct {
	oid   uint32
	since time.Time
}

// unusedClockDB persists the clock; pgUnusedClockDB is the real one.
type unusedClockDB interface {
	load(ctx context.Context) (map[string]clockRow, error)
	// openZeroScan lists the identities of open zero-scan unused_index
	// findings.
	openZeroScan(ctx context.Context) ([]string, error)
	// history returns, newest first, at most one 'indexes' document per
	// day for the days before now, restricted to idents.
	history(ctx context.Context, idents []string, now time.Time,
		days int) ([]indexHistoryDoc, error)
	save(ctx context.Context, upserts map[string]clockRow, deletes []string) error
}

// indexHistoryDoc is one decoded 'indexes' snapshot.
type indexHistoryDoc struct {
	at      time.Time
	indexes map[string]historyIndex
}

type historyIndex struct {
	oid   uint32
	scans int64
}

// unusedClockStore mirrors the persisted clock. held are the open
// zero-scan findings whose clock had not reached the window when the
// store loaded (no persisted evidence, or less than a window of it): they
// stay open until the clock reaches the window or evidence clears them.
type unusedClockStore struct {
	db        unusedClockDB
	loaded    bool
	persisted map[string]clockRow
	held      map[string]clockRow
}

// newUnusedClockStore returns a store on db; without a database the clock
// is in memory only and the store is ready at once.
func newUnusedClockStore(db unusedClockDB) *unusedClockStore {
	return &unusedClockStore{db: db, loaded: db == nil,
		persisted: map[string]clockRow{}, held: map[string]clockRow{}}
}

func (a *Analyzer) unusedWindow() time.Duration {
	return time.Duration(a.cfg.Analyzer.UnusedIndexWindowDays) * 24 * time.Hour
}

// prepareUnusedClock starts the cycle's zero-scan bookkeeping and loads the
// persisted clock until a load succeeds. It waits for a cycle with an
// index list (seeding matches rows to it) and a stats epoch (which decides
// the holds).
func (a *Analyzer) prepareUnusedClock(ctx context.Context, current *collector.Snapshot) {
	a.extras.zeroScanSeen = map[string]bool{}
	s := a.unusedClock
	if s == nil || s.loaded || a.eval.hasFailed("unused_index") ||
		!hasIndexList(current, nil) {
		return
	}
	if err := a.loadUnusedClock(ctx, current); err != nil {
		a.logFn("WARN", "analyzer: load unused index clock: %v; unused_index findings "+
			"are not resolved until it loads (retrying next cycle)", err)
	}
}

func (a *Analyzer) loadUnusedClock(ctx context.Context, current *collector.Snapshot) error {
	s := a.unusedClock
	rows, err := s.db.load(ctx)
	if err != nil {
		return fmt.Errorf("read sage.unused_index_clock: %w", err)
	}
	open, err := s.db.openZeroScan(ctx)
	if err != nil {
		return fmt.Errorf("list open zero-scan unused_index findings: %w", err)
	}
	byIdent := indexesByIdent(current)
	now := a.extras.now()
	seeds, err := a.backfillUnusedClock(ctx, open, rows, byIdent, now)
	if err != nil {
		return err
	}
	seedUnusedClock(a.extras, seeds, byIdent)
	s.persisted = rows
	s.held = heldUnused(open, byIdent, a.extras,
		relationStatsEpoch(current, a.extras), a.unusedWindow(), now)
	s.loaded = true
	return nil
}

// backfillUnusedClock returns the persisted rows plus a clock for each open
// zero-scan finding whose index has none for its current oid (findings
// from before the table existed): the earliest day from which the history
// shows the same object continuously at zero scans, else now.
func (a *Analyzer) backfillUnusedClock(
	ctx context.Context, open []string, rows map[string]clockRow,
	byIdent map[string]collector.IndexStats, now time.Time,
) (map[string]clockRow, error) {
	seeds := make(map[string]clockRow, len(rows)+len(open))
	for k, v := range rows {
		seeds[k] = v
	}
	var missing []string
	for _, ident := range open {
		idx, ok := byIdent[ident]
		if ok && idx.IdxScan == 0 && idx.IndexRelID != 0 && rows[ident].oid != idx.IndexRelID {
			missing = append(missing, ident)
		}
	}
	if len(missing) == 0 {
		return seeds, nil
	}
	docs, err := a.unusedClock.db.history(ctx, missing, now,
		a.cfg.Analyzer.UnusedIndexWindowDays+1)
	if err != nil {
		return nil, fmt.Errorf("read index history for %d open unused_index findings: %w",
			len(missing), err)
	}
	for _, ident := range missing {
		oid := byIdent[ident].IndexRelID
		since, ok := earliestZeroScan(docs, ident, oid)
		if !ok {
			since = now
		}
		seeds[ident] = clockRow{oid: oid, since: since}
	}
	return seeds, nil
}

// earliestZeroScan walks docs (newest first) while they show ident as the
// object oid at zero scans and returns the oldest such document's time.
func earliestZeroScan(docs []indexHistoryDoc, ident string, oid uint32) (time.Time, bool) {
	var since time.Time
	found := false
	for _, d := range docs {
		ix, ok := d.indexes[ident]
		if !ok || ix.oid != oid || ix.scans != 0 {
			break
		}
		since, found = d.at, true
	}
	return since, found
}

func indexesByIdent(snap *collector.Snapshot) map[string]collector.IndexStats {
	out := make(map[string]collector.IndexStats, len(snap.Indexes))
	for _, idx := range snap.Indexes {
		out[idx.SchemaName+"."+idx.IndexRelName] = idx
	}
	return out
}

// seedUnusedClock puts each row whose oid matches the index's current oid
// into the in-memory clock, unless memory already holds an earlier
// sighting of the same object. A row for another object is not evidence.
func seedUnusedClock(e *RuleExtras, rows map[string]clockRow,
	byIdent map[string]collector.IndexStats) {
	if e.FirstSeen == nil {
		e.FirstSeen = map[string]time.Time{}
	}
	if e.IndexOID == nil {
		e.IndexOID = map[string]uint32{}
	}
	for ident, row := range rows {
		idx, ok := byIdent[ident]
		if !ok || row.oid == 0 || idx.IndexRelID != row.oid {
			continue
		}
		if cur, seen := e.FirstSeen[ident]; seen &&
			(e.IndexOID[ident] != row.oid || !row.since.Before(cur)) {
			continue
		}
		e.FirstSeen[ident] = row.since
		e.IndexOID[ident] = row.oid
	}
}

// heldUnused returns the open findings whose index is still at zero scans
// as the same object and whose clock has not reached the window, unless a
// statistics reset after the clock explains it (that resolves, as always).
func heldUnused(open []string, byIdent map[string]collector.IndexStats, e *RuleExtras,
	epoch time.Time, window time.Duration, now time.Time) map[string]clockRow {
	held := map[string]clockRow{}
	for _, ident := range open {
		idx, ok := byIdent[ident]
		since, seen := e.FirstSeen[ident]
		if !ok || !seen || idx.IdxScan != 0 || idx.IndexRelID == 0 ||
			e.IndexOID[ident] != idx.IndexRelID {
			continue
		}
		if now.Sub(since) < window && !epoch.After(since) {
			held[ident] = clockRow{oid: idx.IndexRelID, since: since}
		}
	}
	return held
}

// holdUnusedFindings applies the clock's warmup after the unused_index
// rule ran: while the store has not loaded the category fails, so nothing
// resolves; afterwards each held finding is kept open while its clock is
// the one it was held with. Emission, a scan, a dropped or recreated
// index, a counter decrease, a later reset or an attach to a partitioned
// index end the hold.
func (a *Analyzer) holdUnusedFindings(current, previous *collector.Snapshot, emitted []Finding) {
	s := a.unusedClock
	if s == nil {
		return
	}
	if !s.loaded {
		a.eval.fail("unused_index")
		return
	}
	if len(s.held) == 0 || a.eval.hasFailed("unused_index") || !hasIndexes(current, previous) {
		return
	}
	ended := map[string]bool{}
	for _, f := range emitted {
		ended[f.ObjectIdentifier] = true
	}
	// An attached index is never emitted (rules-03), so its hold ends.
	for _, idx := range current.Indexes {
		if idx.ParentIndexOID != 0 {
			ended[idx.SchemaName+"."+idx.IndexRelName] = true
		}
	}
	epoch := relationStatsEpoch(current, a.extras)
	for ident, h := range s.held {
		since, seen := a.extras.FirstSeen[ident]
		if ended[ident] || !a.extras.zeroScanSeen[ident] || !seen || !since.Equal(h.since) ||
			a.extras.IndexOID[ident] != h.oid || epoch.After(h.since) {
			delete(s.held, ident)
			continue
		}
		a.eval.keep("unused_index", ident)
	}
}

// saveUnusedClock writes the clocks that changed this cycle and deletes
// the forgotten ones, in one statement. A failed write keeps the mirror,
// so the same changes are written next cycle.
func (a *Analyzer) saveUnusedClock(ctx context.Context) {
	s := a.unusedClock
	if s == nil || !s.loaded || s.db == nil {
		return
	}
	up, del := diffUnusedClock(s.persisted, a.extras)
	if len(up) == 0 && len(del) == 0 {
		return
	}
	if err := s.db.save(ctx, up, del); err != nil {
		a.logFn("ERROR", "analyzer: save unused index clock (%d idents changed, %d removed): "+
			"%v; keeping the in-memory clocks and retrying next cycle", len(up), len(del), err)
		return
	}
	for k, v := range up {
		s.persisted[k] = v
	}
	for _, k := range del {
		delete(s.persisted, k)
	}
}

// diffUnusedClock compares the in-memory clock with the persisted rows.
// Clocks of an unknown object (oid 0) are never persisted.
func diffUnusedClock(persisted map[string]clockRow, e *RuleExtras) (map[string]clockRow,
	[]string) {
	up := map[string]clockRow{}
	for ident, since := range e.FirstSeen {
		oid := e.IndexOID[ident]
		if oid == 0 {
			continue
		}
		if p, ok := persisted[ident]; !ok || p.oid != oid || !p.since.Equal(since) {
			up[ident] = clockRow{oid: oid, since: since}
		}
	}
	var del []string
	for ident := range persisted {
		if _, ok := e.FirstSeen[ident]; !ok || e.IndexOID[ident] == 0 {
			del = append(del, ident)
		}
	}
	sort.Strings(del)
	return up, del
}
