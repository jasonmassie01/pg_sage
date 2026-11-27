package collector

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// collection-11: self-exclusion runs in Go on the candidates' texts and
// the list is cut to MaxQueries after it.

// Boundary: of 2N candidates, exactly N application queries survive, in
// rank order, whatever share of them is pg_sage's own.
func TestQueryTexts_ExactlyNSurviveExclusion(t *testing.T) {
	c, _ := textCollector(3)
	texts := appTexts(1, 3, 5, 6)
	texts[2] = "/* pg_sage */ SELECT count(*) FROM pg_stat_activity"
	texts[4] = "SELECT * FROM sage.findings WHERE id = $1"
	db := newFakeStatements(texts)
	db.candidates(1, 2, 3, 4, 5, 6)
	qs := mustCollect(t, c, db)
	if !slices.Equal(idsOf(qs), []int64{1, 3, 5}) {
		t.Fatalf("queries = %v, want the first 3 application ones [1 3 5]", idsOf(qs))
	}
}

// Boundary: when all 2N candidates are pg_sage's own, zero queries result.
func TestQueryTexts_AllCandidatesSelfGiveNone(t *testing.T) {
	c, _ := textCollector(2)
	db := newFakeStatements(map[int64]any{
		1: "/* pg_sage */ SELECT 1", 2: "SELECT /* pg_sage */ 2",
		3: `SELECT * FROM "sage".x`, 4: "INSERT INTO SAGE.snapshots VALUES ($1)",
	})
	db.candidates(1, 2, 3, 4)
	qs := mustCollect(t, c, db)
	if qs == nil || len(qs) != 0 {
		t.Fatalf("queries = %#v, want empty and non-nil", qs)
	}
}

// Invalid input: a candidate whose text is NULL (unreadable text file) or
// gone (evicted between the reads) cannot be attributed, so it is left out
// (fail closed: it may be pg_sage's own) and not cached.
func TestQueryTexts_UnreadableTextLeftOutAndNotCached(t *testing.T) {
	c, _ := textCollector(5)
	texts := appTexts(1)
	texts[2] = nil
	db := newFakeStatements(texts)
	db.candidates(1, 2, 3) // 3 has no text row at all
	qs := mustCollect(t, c, db)
	if !slices.Equal(idsOf(qs), []int64{1}) {
		t.Fatalf("queries = %v, want only [1] with a readable text", idsOf(qs))
	}
	mustCollect(t, c, db)
	if got := db.reads(); len(got) != 2 || !slices.Equal(got[1], []int64{2, 3}) {
		t.Fatalf("text reads = %v, want [2 3] asked again next cycle", got)
	}
}

// Concurrent access: cycles of one collector may overlap with readers of
// its cache; every result keeps its own texts and the cache stays bounded.
func TestQueryTexts_ConcurrentCollects(t *testing.T) {
	c, _ := textCollector(4)
	db := newFakeStatements(appTexts(1, 2, 3, 4, 5, 6, 7, 8))
	db.candidates(1, 2, 3, 4, 5, 6, 7, 8)
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				qs, err := c.collectQueriesVia(context.Background(), db)
				if err != nil || !slices.Equal(idsOf(qs), []int64{1, 2, 3, 4}) ||
					qs[2].Query != appTexts(3)[3] {
					errs <- "inconsistent result"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if n := c.stmtTexts.len(); n != 8 {
		t.Fatalf("cache holds %d entries, want the 8 candidates", n)
	}
}

// State transitions of the near-capacity classification: inside the
// interval the counts are reused; after it they are reread only when the
// entry count or a known deallocation count moved.
func TestClassificationCurrent(t *testing.T) {
	at := time.Unix(9_000_000, 0)
	cached := classCache{at: at, entries: 4800, dealloc: 100}
	cases := []struct {
		name    string
		cached  classCache
		now     time.Time
		entries int
		dealloc int64
		want    bool
	}{
		{"never classified", classCache{}, at, 4800, 100, false},
		{"inside the interval", cached, at.Add(classifyEvery - time.Second), 4900, 101, true},
		{"after it, unchanged", cached, at.Add(classifyEvery), 4800, 100, true},
		{"after it, more entries", cached, at.Add(classifyEvery), 4801, 100, false},
		{"after it, more deallocations", cached, at.Add(classifyEvery), 4800, 101, false},
		{"after it, dealloc unknown", classCache{at: at, entries: 4800, dealloc: -1},
			at.Add(classifyEvery), 4800, -1, false},
	}
	for _, tc := range cases {
		u := &StatStatementsUsage{Entries: tc.entries, Dealloc: tc.dealloc}
		if got := classificationCurrent(tc.cached, tc.now, u); got != tc.want {
			t.Errorf("%s: current = %v, want %v", tc.name, got, tc.want)
		}
	}
	if classifyEvery != time.Hour {
		t.Fatalf("classifyEvery = %s, want 1h", classifyEvery)
	}
}
