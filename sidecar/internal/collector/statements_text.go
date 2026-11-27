package collector

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// pg_stat_statements(true) loads the whole query-text file, whatever rows
// the statement keeps (about 14 MB on lifeos, 265-325 ms of the queries
// read measured on 2026-10-04), and every collector on a cluster read it
// every cycle. The queries step ranks the counters without texts and
// takes the candidates' texts from this cache (collection-11). A text is
// read only when a candidate is new to the cache or due a refresh, so a
// steady workload reads them at most once an hour. Each collector (one per
// database, also in fleet mode) keeps its own cache, keyed by queryid of
// its own database: nothing is shared between databases.

const (
	// stmtTextRefreshEvery is the age at which a cached text is read again.
	stmtTextRefreshEvery = time.Hour
	// stmtTextMaxBytes caps the texts one collector keeps.
	stmtTextMaxBytes = 32 << 20
	// stmtTextEntriesPerQuery x MaxQueries caps the entries it keeps.
	stmtTextEntriesPerQuery = 4
)

// statementTextsTag names the text read in pg_stat_statements, so its cost
// and frequency can be measured apart from the collector's other reads.
const statementTextsTag = "/* pg_sage text */"

// statementTextsSQL reads the texts of the given queryids ($1) of this
// database, one per queryid: the text of its most-called row, as the
// single-statement read before collection-11 kept. fn is the qualified
// pg_stat_statements function.
func statementTextsSQL(fn string) string {
	return statementTextsTag + `
SELECT queryid, (array_agg(query ORDER BY calls DESC))[1]
  FROM ` + fn + `(true)
 WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND queryid = ANY($1)
 GROUP BY queryid`
}

// stmtText is one cached text: when it was read, the last cycle its
// queryid was a candidate (for least-recently-seen eviction), and whether
// it is pg_sage's own statement (selfmonitor.IsStatementText).
type stmtText struct {
	text   string
	readAt time.Time
	seen   uint64
	own    bool
}

// stmtTextCache is a collector's cache of statement texts by queryid.
type stmtTextCache struct {
	mu       sync.Mutex
	entries  map[int64]*stmtText
	bytes    int
	cycle    uint64
	epoch    time.Time // last known statistics epoch
	dealloc  int64     // last known pg_stat_statements_info.dealloc; -1 unknown
	maxBytes int
}

func newStmtTextCache() *stmtTextCache {
	return &stmtTextCache{entries: map[int64]*stmtText{}, dealloc: -1,
		maxBytes: stmtTextMaxBytes}
}

// observeEpoch clears the cache when the statistics epoch changed (a
// reset or a restart). An unknown (zero) epoch is no evidence of a reset.
func (s *stmtTextCache) observeEpoch(epoch time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if epochChanged(s.epoch, epoch) {
		clear(s.entries)
		s.bytes = 0
	}
	if !epoch.IsZero() {
		s.epoch = epoch
	}
}

// observeDealloc queues a refresh of every cached text when entries were
// deallocated since the last known count; an unknown (-1) count is
// skipped.
func (s *stmtTextCache) observeDealloc(dealloc int64) {
	if dealloc < 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dealloc >= 0 && dealloc > s.dealloc {
		for _, e := range s.entries {
			e.readAt = time.Time{}
		}
	}
	s.dealloc = dealloc
}

// lookup starts a cycle: it marks ids seen and returns the cached texts
// and, sorted, the ids to read (not cached or due a refresh).
func (s *stmtTextCache) lookup(ids []int64, now time.Time) (map[int64]string, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cycle++
	texts := make(map[int64]string, len(ids))
	var missing []int64
	for _, id := range ids {
		e, ok := s.entries[id]
		if ok {
			e.seen = s.cycle
		}
		if !ok || e.readAt.IsZero() || now.Sub(e.readAt) >= stmtTextRefreshEvery {
			missing = append(missing, id)
			continue
		}
		texts[id] = e.text
	}
	slices.Sort(missing)
	return texts, missing
}

// ownIDs is the cached queryids whose text is pg_sage's own, sorted: the
// ranked read keeps them out of its mean and io arms (collection-07).
func (s *stmtTextCache) ownIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := []int64{}
	for id, e := range s.entries {
		if e.own {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// store caches texts read at now, then evicts the least recently seen
// entries past maxEntries or the byte cap.
func (s *stmtTextCache) store(texts map[int64]string, now time.Time, maxEntries int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, text := range texts {
		if old, ok := s.entries[id]; ok {
			s.bytes -= len(old.text)
		}
		s.entries[id] = &stmtText{text: text, readAt: now, seen: s.cycle,
			own: selfmonitor.IsStatementText(text)}
		s.bytes += len(text)
	}
	if len(s.entries) <= maxEntries && s.bytes <= s.maxBytes {
		return
	}
	ids := make([]int64, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b int64) int {
		if ea, eb := s.entries[a], s.entries[b]; ea.seen != eb.seen {
			return cmp.Compare(ea.seen, eb.seen)
		}
		return cmp.Compare(a, b)
	})
	for _, id := range ids {
		if len(s.entries) <= maxEntries && s.bytes <= s.maxBytes {
			break
		}
		s.bytes -= len(s.entries[id].text)
		delete(s.entries, id)
	}
}

func (s *stmtTextCache) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *stmtTextCache) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// resolve returns the texts of ids, reading the ones the cache lacks in
// one statement. A failed read caches nothing and fails the step.
func (s *stmtTextCache) resolve(ctx context.Context, db catalogQuerier, fn string,
	ids []int64, now time.Time, maxEntries int) (map[int64]string, error) {
	texts, missing := s.lookup(ids, now)
	if len(missing) == 0 {
		return texts, nil
	}
	read, err := readStatementTexts(ctx, db, fn, missing)
	if err != nil {
		return nil, fmt.Errorf("read pg_stat_statements texts of %d queries: %w",
			len(missing), err)
	}
	s.store(read, now, maxEntries)
	for id, text := range read {
		texts[id] = text
	}
	return texts, nil
}

// readStatementTexts reads the texts of ids. A NULL text (the text file
// could not be read) is not returned, so it is neither attributed nor
// cached.
func readStatementTexts(ctx context.Context, db catalogQuerier, fn string,
	ids []int64) (map[int64]string, error) {
	rows, err := db.Query(ctx, statementTextsSQL(fn), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	texts := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var text *string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		if text != nil {
			texts[id] = *text
		}
	}
	return texts, rows.Err()
}
