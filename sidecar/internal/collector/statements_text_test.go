package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// collection-11: the queries step ranks pg_stat_statements on its
// counters alone and reads texts only for candidates its per-collector
// cache does not hold (or holds past their refresh). These tests need no
// database: fakeStatements answers both reads and counts the text reads.

// fakeStatements answers the counters read and the text read of the
// queries step. texts maps a queryid to its text (nil is a NULL text).
type fakeStatements struct {
	mu        sync.Mutex
	counters  [][]any
	texts     map[int64]any
	countErr  error
	textErr   error
	textReads [][]int64
}

func (f *fakeStatements) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(sql, `pg_stat_statements"(true)`) {
		if f.countErr != nil {
			return nil, f.countErr
		}
		return &fakeRows{rows: f.counters}, nil
	}
	ids, ok := args[0].([]int64)
	if !ok {
		return nil, fmt.Errorf("text read argument is %T, want []int64", args[0])
	}
	f.textReads = append(f.textReads, slices.Clone(ids))
	if f.textErr != nil {
		return nil, f.textErr
	}
	var rows [][]any
	for _, id := range ids {
		if text, ok := f.texts[id]; ok {
			rows = append(rows, []any{id, text})
		}
	}
	return &fakeRows{rows: rows}, nil
}

func (f *fakeStatements) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeRow{err: errors.New("fakeStatements: unexpected QueryRow")}
}

func (f *fakeStatements) reads() [][]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.textReads)
}

// candidates sets the counters read to ids, ranked in that order.
func (f *fakeStatements) candidates(ids ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counters = nil
	for i, id := range ids {
		f.counters = append(f.counters, counterRow(id, float64(1000-i)))
	}
}

// The database's statement totals every counters row carries (evid-01a).
const (
	counterTotalsExecMs  = 5000.5
	counterTotalsCalls   = int64(321)
	counterTotalsEntries = 17
)

// counterRow is one row of the base counters read (no WAL, no plan time),
// admitted by the total ranking (collection-07 added the admitted_by
// column, evid-01a the three statement totals before it).
func counterRow(id int64, total float64) []any {
	return []any{id, int64(10), total, total / 10, 0.5, total / 2, 1.5, int64(7),
		int64(1), int64(2), int64(3), int64(4), int64(5), int64(6), 0.75, 0.25,
		counterTotalsExecMs, counterTotalsCalls, counterTotalsEntries,
		[]string{"total"}}
}

func newFakeStatements(texts map[int64]any) *fakeStatements {
	return &fakeStatements{texts: texts}
}

// appTexts maps each id to an application statement text.
func appTexts(ids ...int64) map[int64]any {
	m := map[int64]any{}
	for _, id := range ids {
		m[id] = fmt.Sprintf("SELECT * FROM app_t%d WHERE id = $1", id)
	}
	return m
}

// textCollector is a collector with fixed base capabilities, a settable
// clock and limit as MaxQueries.
func textCollector(limit int) (*Collector, *time.Time) {
	cfg := testConfig()
	cfg.Collector.MaxQueries = limit
	cfg.HasWALColumns, cfg.HasPlanTimeColumns = false, false
	c := New(nil, cfg, 170000, noopLog)
	c.caps.attempted = true
	clock := time.Unix(7_000_000, 0)
	c.now = func() time.Time { return clock }
	return c, &clock
}

func mustCollect(t *testing.T, c *Collector, db catalogQuerier) []QueryStats {
	t.Helper()
	qs, err := c.collectQueriesVia(context.Background(), db)
	if err != nil {
		t.Fatalf("collect queries: %v", err)
	}
	return qs
}

func idsOf(qs []QueryStats) []int64 {
	ids := make([]int64, 0, len(qs))
	for _, q := range qs {
		ids = append(ids, q.QueryID)
	}
	return ids
}

// Happy path: a cache holding every candidate reads no text.
func TestQueryTexts_FullCacheReadsNoText(t *testing.T) {
	c, _ := textCollector(5)
	db := newFakeStatements(appTexts(1, 2, 3))
	db.candidates(1, 2, 3)
	first := mustCollect(t, c, db)
	second := mustCollect(t, c, db)
	if got := db.reads(); len(got) != 1 || !slices.Equal(got[0], []int64{1, 2, 3}) {
		t.Fatalf("text reads = %v, want one read of [1 2 3]", got)
	}
	for _, qs := range [][]QueryStats{first, second} {
		if !slices.Equal(idsOf(qs), []int64{1, 2, 3}) {
			t.Fatalf("queries = %v, want [1 2 3] in rank order", idsOf(qs))
		}
		if qs[1].Query != "SELECT * FROM app_t2 WHERE id = $1" || qs[1].Calls != 10 ||
			qs[1].TotalExecTime != 999 || qs[1].Rows != 7 || qs[1].BlkWriteTime != 0.25 {
			t.Fatalf("query 2 = %+v, want its text and counters", qs[1])
		}
	}
}

// Missing ids give exactly one read with exactly those ids.
func TestQueryTexts_MissingIDsReadOnceWithThoseIDs(t *testing.T) {
	c, _ := textCollector(5)
	db := newFakeStatements(appTexts(1, 2, 3, 4))
	db.candidates(1, 2)
	mustCollect(t, c, db)
	db.candidates(4, 1, 3, 2)
	qs := mustCollect(t, c, db)
	got := db.reads()
	if len(got) != 2 || !slices.Equal(got[1], []int64{3, 4}) {
		t.Fatalf("text reads = %v, want a second read of exactly [3 4]", got)
	}
	if !slices.Equal(idsOf(qs), []int64{4, 1, 3, 2}) || qs[0].Query != appTexts(4)[4] {
		t.Fatalf("queries = %v (%q), want [4 1 3 2] with texts", idsOf(qs), qs[0].Query)
	}
}

// State transition: a new statistics epoch clears the cache; an unknown
// (zero) epoch is no evidence of a reset.
func TestQueryTexts_EpochChangeClearsCache(t *testing.T) {
	c, _ := textCollector(5)
	db := newFakeStatements(appTexts(1, 2))
	db.candidates(1, 2)
	e1 := time.Unix(1_000, 0)
	c.stmtTexts.observeEpoch(e1)
	mustCollect(t, c, db)
	c.stmtTexts.observeEpoch(time.Time{})
	mustCollect(t, c, db)
	c.stmtTexts.observeEpoch(e1)
	mustCollect(t, c, db)
	if n := len(db.reads()); n != 1 {
		t.Fatalf("%d text reads with one known epoch, want 1", n)
	}
	c.stmtTexts.observeEpoch(e1.Add(time.Second))
	mustCollect(t, c, db)
	if got := db.reads(); len(got) != 2 || !slices.Equal(got[1], []int64{1, 2}) {
		t.Fatalf("text reads after the epoch changed = %v, want all of [1 2] again", got)
	}
}

// State transition: rising deallocations queue a refresh of the cached
// texts; an unchanged or unknown count does not.
func TestQueryTexts_DeallocGrowthQueuesRefresh(t *testing.T) {
	c, _ := textCollector(5)
	db := newFakeStatements(appTexts(1, 2))
	db.candidates(1, 2)
	c.stmtTexts.observeDealloc(5)
	mustCollect(t, c, db)
	c.stmtTexts.observeDealloc(5)
	mustCollect(t, c, db)
	c.stmtTexts.observeDealloc(-1)
	mustCollect(t, c, db)
	if n := len(db.reads()); n != 1 {
		t.Fatalf("%d text reads with dealloc unchanged or unknown, want 1", n)
	}
	c.stmtTexts.observeDealloc(6)
	mustCollect(t, c, db)
	mustCollect(t, c, db)
	if got := db.reads(); len(got) != 2 || !slices.Equal(got[1], []int64{1, 2}) {
		t.Fatalf("text reads after dealloc rose = %v, want one refresh of [1 2]", got)
	}
}

// Boundary: every entry is refreshed once it is an hour old, including a
// text the role may not see.
func TestQueryTexts_HourlyRefreshIncludingInsufficientPrivilege(t *testing.T) {
	c, clock := textCollector(5)
	texts := appTexts(1)
	texts[2] = "<insufficient privilege>"
	db := newFakeStatements(texts)
	db.candidates(1, 2)
	first := mustCollect(t, c, db)
	*clock = clock.Add(stmtTextRefreshEvery - time.Second)
	mustCollect(t, c, db)
	if n := len(db.reads()); n != 1 {
		t.Fatalf("%d text reads inside the hour, want 1", n)
	}
	*clock = clock.Add(time.Second)
	mustCollect(t, c, db)
	if got := db.reads(); len(got) != 2 || !slices.Equal(got[1], []int64{1, 2}) {
		t.Fatalf("text reads at the hour = %v, want a refresh of [1 2]", got)
	}
	if len(first) != 2 || first[1].Query != "<insufficient privilege>" {
		t.Fatalf("queries = %+v, want the unreadable text kept as reported", first)
	}
	if stmtTextRefreshEvery != time.Hour {
		t.Fatalf("refresh every %s, want 1h", stmtTextRefreshEvery)
	}
}

// Boundary: the cache holds at most 4 x MaxQueries entries and evicts the
// least recently seen.
func TestQueryTexts_EvictionHoldsAtFourTimesMaxQueries(t *testing.T) {
	c, _ := textCollector(2) // 2N = 4 candidates per cycle, 4N = 8 entries
	all := appTexts(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12)
	db := newFakeStatements(all)
	for cycle := int64(0); cycle < 3; cycle++ {
		b := cycle*4 + 1
		db.candidates(b, b+1, b+2, b+3)
		mustCollect(t, c, db)
	}
	if n := c.stmtTexts.len(); n != 8 {
		t.Fatalf("cache holds %d entries, want 8 (4 x MaxQueries)", n)
	}
	db.candidates(5, 6, 7, 8) // the second cycle's: still cached
	mustCollect(t, c, db)
	db.candidates(1, 2, 3, 4) // the first cycle's: evicted
	mustCollect(t, c, db)
	got := db.reads()
	if len(got) != 4 || !slices.Equal(got[3], []int64{1, 2, 3, 4}) {
		t.Fatalf("text reads = %v, want 4 with the evicted [1 2 3 4] read again", got)
	}
}

// Boundary: the cache holds at most maxBytes of text, evicting the least
// recently seen first.
func TestQueryTexts_EvictionHoldsAtByteCap(t *testing.T) {
	if stmtTextMaxBytes != 32<<20 {
		t.Fatalf("byte cap = %d, want 32 MiB", stmtTextMaxBytes)
	}
	cache := newStmtTextCache()
	cache.maxBytes = 100
	now := time.Unix(1, 0)
	text := strings.Repeat("x", 40)
	for id := int64(1); id <= 3; id++ {
		cache.lookup([]int64{id}, now)
		cache.store(map[int64]string{id: text}, now, 1000)
	}
	if cache.len() != 2 || cache.size() != 80 {
		t.Fatalf("cache holds %d entries, %d bytes, want 2 entries of 80 bytes",
			cache.len(), cache.size())
	}
	_, missing := cache.lookup([]int64{1, 2, 3}, now)
	if !slices.Equal(missing, []int64{1}) {
		t.Fatalf("missing = %v, want the least recently seen [1] evicted", missing)
	}
}

// Nil/empty: no candidates read no text and give an empty, non-nil list.
func TestQueryTexts_NoCandidatesNoTextRead(t *testing.T) {
	c, _ := textCollector(5)
	db := newFakeStatements(nil)
	qs := mustCollect(t, c, db)
	if qs == nil || len(qs) != 0 || len(db.reads()) != 0 {
		t.Fatalf("queries = %#v after %d text reads, want empty non-nil and none",
			qs, len(db.reads()))
	}
}

// Error propagation: a failed text read fails the step with its cause
// (still retryable), publishes no counters and caches nothing.
func TestQueryTexts_FailedTextReadFailsStep(t *testing.T) {
	c, _ := textCollector(5)
	db := newFakeStatements(appTexts(1, 2))
	db.candidates(1, 2)
	db.textErr = &pgconn.PgError{Code: "57014", Message: "canceling statement due to " +
		"statement timeout"}
	qs, err := c.collectQueriesVia(context.Background(), db)
	if qs != nil || err == nil {
		t.Fatalf("queries = %v, err = %v, want nil and the text read's error", qs, err)
	}
	if !strings.Contains(err.Error(), "texts") || !catalogread.Retryable(err) {
		t.Fatalf("err = %v, want it to name the text read and stay retryable", err)
	}
	db.textErr = nil
	mustCollect(t, c, db)
	if got := db.reads(); len(got) != 2 || !slices.Equal(got[1], []int64{1, 2}) {
		t.Fatalf("text reads = %v, want the failed ids read again", got)
	}
}

// Error propagation: a failed counters read reads no text.
func TestQueryTexts_FailedCountersReadFailsStep(t *testing.T) {
	c, _ := textCollector(5)
	db := newFakeStatements(appTexts(1))
	db.countErr = errors.New("permission denied for function pg_stat_statements")
	qs, err := c.collectQueriesVia(context.Background(), db)
	if qs != nil || err == nil || !strings.Contains(err.Error(), "permission denied") ||
		!strings.Contains(err.Error(), "counters") || len(db.reads()) != 0 {
		t.Fatalf("queries = %v, err = %v, reads %v; want the counters error, no read",
			qs, err, db.reads())
	}
}
