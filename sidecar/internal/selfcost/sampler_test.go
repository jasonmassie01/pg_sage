package selfcost

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func sampleOK(t *testing.T, s *Sampler, db *fakeDB) map[StatementKey]StatementCounters {
	t.Helper()
	got, known, err := s.Statements(context.Background(), db)
	if err != nil || !known {
		t.Fatalf("Statements: known=%v err=%v", known, err)
	}
	return got
}

func skey(id int64) StatementKey { return StatementKey{UserID: 10, QueryID: id, TopLevel: true} }

// Performance: the query texts are read only when keys never classified
// appear. A second sample with the same keys reads counters alone; a new
// tagged statement costs exactly one text read, and so does a new
// application statement (it has to be told apart once).
func TestSampler_TextOnlyForUnclassifiedKeys(t *testing.T) {
	db := newFakeDB("1.11")
	db.add(1, true, 5)
	db.add(2, false, 9)
	s := NewSampler(nil)
	first := sampleOK(t, s, db)
	if db.count("texts") != 1 || len(first) != 1 || first[skey(1)].Calls != 5 ||
		!strings.Contains(first[skey(1)].Text, "sage_1") {
		t.Fatalf("first sample = %+v with %d text reads, want sage_1 only after one",
			first, db.count("texts"))
	}
	db.set(1, 8)
	second := sampleOK(t, s, db)
	if db.count("texts") != 1 {
		t.Fatalf("second sample with no new keys read texts (%d reads)", db.count("texts"))
	}
	if c := second[skey(1)]; c.Calls != 8 || c.TimeMs != 16 || c.Blocks != 24 ||
		!strings.Contains(c.Text, "sage_1") {
		t.Fatalf("second sample's counters = %+v, want calls 8 with the cached text", c)
	}
	db.add(3, true, 1)
	third := sampleOK(t, s, db)
	if db.count("texts") != 2 || third[skey(3)].Calls != 1 || len(third) != 2 {
		t.Fatalf("new tagged key: %d text reads, sample %+v", db.count("texts"), third)
	}
	db.add(4, false, 1)
	if got := sampleOK(t, s, db); db.count("texts") != 3 || len(got) != 2 {
		t.Fatalf("new application key: %d text reads, sample %+v", db.count("texts"), got)
	}
	sampleOK(t, s, db)
	if db.count("texts") != 3 || db.count("counters") != 5 || db.count("version") != 1 {
		t.Fatalf("steady state reads: %v, want texts 3, counters 5, version once", db.calls)
	}
}

// State transition: an entry whose calls went back was reset or evicted
// and re-created; its classification is read again, and the counters are
// the new entry's.
func TestSampler_CallsDecreaseRevalidates(t *testing.T) {
	db := newFakeDB("1.9")
	db.add(1, true, 10)
	s := NewSampler(nil)
	sampleOK(t, s, db)
	db.set(1, 3)
	got := sampleOK(t, s, db)
	if db.count("texts") != 2 || got[skey(1)].Calls != 3 {
		t.Fatalf("after calls went back: %d text reads, sample %+v", db.count("texts"), got)
	}
}

// State transition: pg_stat_statements_reset() moves stats_reset; every
// key is read again even when its calls happen to have grown past the old
// value.
func TestSampler_StatsResetRevalidates(t *testing.T) {
	db := newFakeDB("1.10")
	r1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	db.reset = &r1
	db.add(1, true, 1)
	db.add(2, false, 1)
	s := NewSampler(nil)
	sampleOK(t, s, db)
	db.set(1, 50)
	sampleOK(t, s, db)
	if db.count("texts") != 1 {
		t.Fatalf("no reset: %d text reads, want 1", db.count("texts"))
	}
	r2 := r1.Add(time.Hour)
	db.reset = &r2
	db.set(1, 60)
	got := sampleOK(t, s, db)
	if db.count("texts") != 2 || got[skey(1)].Calls != 60 {
		t.Fatalf("after stats_reset moved: %d text reads, sample %+v", db.count("texts"), got)
	}
	sampleOK(t, s, db)
	if db.count("texts") != 2 {
		t.Fatalf("same stats_reset again re-read texts (%d)", db.count("texts"))
	}
}

// Eviction: a key gone from pg_stat_statements leaves the cache and the
// sample; when it comes back it is classified again.
func TestSampler_EvictedKeyLeavesCache(t *testing.T) {
	db := newFakeDB("1.11")
	db.add(1, true, 4)
	db.add(2, false, 4)
	s := NewSampler(nil)
	sampleOK(t, s, db)
	db.remove(1)
	db.remove(2)
	if got := sampleOK(t, s, db); len(got) != 0 || len(s.keys) != 0 {
		t.Fatalf("evicted keys still sampled %+v or cached %d", got, len(s.keys))
	}
	db.add(1, true, 1)
	if got := sampleOK(t, s, db); db.count("texts") != 2 || got[skey(1)].Calls != 1 {
		t.Fatalf("returning key: %d text reads, sample %+v", db.count("texts"), got)
	}
}

// Boundary: the cache never holds more than MaxClassifiedKeys keys, and
// pg_sage's own keys are kept (their text is what findings show) even
// when application keys fill it.
func TestSampler_KeyCacheIsBounded(t *testing.T) {
	db := newFakeDB("1.11")
	for id := int64(1); id <= MaxClassifiedKeys+500; id++ {
		db.add(id, false, 1)
	}
	db.add(-1, true, 2)
	s := NewSampler(nil)
	got := sampleOK(t, s, db)
	if len(s.keys) > MaxClassifiedKeys || len(got) != 1 || got[skey(-1)].Calls != 2 {
		t.Fatalf("cache %d keys (max %d), sample %+v", len(s.keys), MaxClassifiedKeys, got)
	}
	db.add(-2, true, 3)
	got = sampleOK(t, s, db)
	if len(s.keys) > MaxClassifiedKeys || len(got) != 2 || got[skey(-2)].Calls != 3 ||
		!s.keys[skey(-1)].sage || !s.keys[skey(-2)].sage {
		t.Fatalf("new tagged key with a full cache: cache %d, sample %+v", len(s.keys), got)
	}
	db.set(-1, 7)
	if got = sampleOK(t, s, db); got[skey(-1)].Calls != 7 ||
		!strings.Contains(got[skey(-1)].Text, "sage_-1") {
		t.Fatalf("cached tagged key lost its text or counters: %+v", got[skey(-1)])
	}
}

// pg_stat_statements 1.8 has no toplevel column and no
// pg_stat_statements_info: neither is read, and statements are measured.
func TestSampler_Version18ReadsWithoutTopLevel(t *testing.T) {
	db := newFakeDB("1.8")
	db.add(1, true, 2)
	got := sampleOK(t, NewSampler(nil), db)
	if len(got) != 1 || got[StatementKey{UserID: 10, QueryID: 1}].Calls != 2 {
		t.Fatalf("1.8 sample = %+v, want the entry with toplevel false", got)
	}
	for _, sql := range db.sent {
		if strings.Contains(sql, "toplevel") || strings.Contains(sql, "pg_stat_statements_info") {
			t.Fatalf("1.8 was sent a 1.9 column: %s", sql)
		}
	}
}

// Versions compare as numbers: 1.10 is newer than 1.9.
func TestSampler_Version110UsesTopLevel(t *testing.T) {
	db := newFakeDB("1.10")
	db.add(1, true, 2)
	if got := sampleOK(t, NewSampler(nil), db); got[skey(1)].Calls != 2 {
		t.Fatalf("1.10 sample = %+v, want the toplevel entry", got)
	}
}

// pg_stat_statements before 1.8 lacks total_exec_time: the statements are
// unknown (no error, nothing but the version queried), and the update is
// advised once. The version is read again each sample until it is updated.
func TestSampler_OldExtensionIsUnknownWithOneNotice(t *testing.T) {
	db := newFakeDB("1.7")
	logs := &noticeLog{}
	s := NewSampler(logs.log)
	for i := 0; i < 3; i++ {
		got, known, err := s.Statements(context.Background(), db)
		if err != nil || known || got != nil {
			t.Fatalf("1.7: sample %v known=%v err=%v, want unknown without error",
				got, known, err)
		}
	}
	if n := logs.matching("ALTER EXTENSION pg_stat_statements UPDATE"); n != 1 {
		t.Fatalf("update notice logged %d times, want once: %v", n, logs.lines)
	}
	if db.count("counters") != 0 || db.count("texts") != 0 || db.count("version") != 3 {
		t.Fatalf("1.7: reads %v, want only the version, once per sample", db.calls)
	}
	// The advised update is seen at the next sample, without a restart.
	db.version = "1.11"
	db.add(1, true, 1)
	if got := sampleOK(t, s, db); got[skey(1)].Calls != 1 {
		t.Fatalf("after the update: %+v", got)
	}
}

// 42703 (a column the installed version lacks) is unavailable, not an
// error; the version is read again next time, and the notice is once.
func TestSampler_UndefinedColumnIsUnknownAndRechecksVersion(t *testing.T) {
	db := newFakeDB("1.9")
	db.countersErr = &pgconn.PgError{Code: "42703", Message: `column "toplevel" does not exist`}
	logs := &noticeLog{}
	s := NewSampler(logs.log)
	for i := 0; i < 2; i++ {
		if got, known, err := s.Statements(context.Background(), db); err != nil || known ||
			got != nil {
			t.Fatalf("42703: sample %v known=%v err=%v", got, known, err)
		}
	}
	if db.count("version") != 2 {
		t.Fatalf("version read %d times, want again after 42703", db.count("version"))
	}
	if n := logs.matching("ALTER EXTENSION pg_stat_statements UPDATE"); n != 1 {
		t.Fatalf("update notice logged %d times, want once", n)
	}
	db.countersErr = nil
	db.add(1, true, 1)
	if got := sampleOK(t, s, db); got[skey(1)].Calls != 1 {
		t.Fatalf("after the update: %+v", got)
	}
}

// Not installed, not preloaded, function missing: unknown, not an error.
func TestSampler_UnavailableIsUnknown(t *testing.T) {
	db := newFakeDB("")
	if got, known, err := NewSampler(nil).Statements(context.Background(), db); err != nil ||
		known || got != nil || db.count("counters") != 0 {
		t.Fatalf("not installed: %v known=%v err=%v calls %v", got, known, err, db.calls)
	}
	for _, code := range []string{"42P01", "55000", "42883"} {
		db := newFakeDB("1.11")
		db.countersErr = &pgconn.PgError{Code: code}
		s := NewSampler(nil)
		if got, known, err := s.Statements(context.Background(), db); err != nil || known ||
			got != nil {
			t.Errorf("%s: %v known=%v err=%v", code, got, known, err)
		}
		db.countersErr = nil
		s.Statements(context.Background(), db)
		if db.count("version") != 2 {
			t.Errorf("%s: version not read again after the extension went away", code)
		}
	}
}

// Error propagation: other failures of each statement are errors that
// keep the server's code and name what was read.
func TestSampler_ErrorsPropagate(t *testing.T) {
	timeout := &pgconn.PgError{Code: "57014", Message: "canceling statement"}
	for name, setup := range map[string]func(*fakeDB){
		"version":  func(db *fakeDB) { db.versionErr = timeout },
		"counters": func(db *fakeDB) { db.countersErr = timeout },
		"texts":    func(db *fakeDB) { db.textsErr = timeout },
	} {
		db := newFakeDB("1.11")
		db.add(1, true, 1)
		setup(db)
		got, known, err := NewSampler(nil).Statements(context.Background(), db)
		var pgErr *pgconn.PgError
		if err == nil || known || got != nil || !errors.As(err, &pgErr) ||
			pgErr.Code != "57014" || !strings.Contains(err.Error(), "pg_stat_statements") {
			t.Errorf("%s failure: sample %v known=%v err=%v", name, got, known, err)
		}
	}
}

// A key that appears between the counters and the texts is counted from
// the text read; one that a stale version read reports twice is summed.
func TestSampler_KeysOnlyInTextsAndDuplicates(t *testing.T) {
	db := newFakeDB("1.11")
	db.add(1, true, 2)
	s := NewSampler(nil)
	sampleOK(t, s, db)
	db.add(2, true, 4)
	db.add(3, false, 1) // unclassified: makes this sample read the texts
	db.textsOnly = map[int64]bool{2: true}
	if got := sampleOK(t, s, db); got[skey(2)].Calls != 4 || got[skey(1)].Calls != 2 ||
		len(got) != 2 {
		t.Fatalf("key only in the text read: %+v", got)
	}
	db = newFakeDB("1.8")
	db.add(1, true, 2)
	db.entries = append(db.entries, fakeEntry{key: StatementKey{UserID: 10, QueryID: 1},
		c:    StatementCounters{TimeMs: 1, Calls: 3, Blocks: 1, Text: "/* pg_sage */ x"},
		sage: true})
	got := sampleOK(t, NewSampler(nil), db)
	if c := got[StatementKey{UserID: 10, QueryID: 1}]; c.Calls != 5 || c.TimeMs != 5 {
		t.Fatalf("duplicate key = %+v, want summed calls 5 and time 5", c)
	}
}

// Nil: a nil sampler samples without a cache.
func TestSampler_NilSamplesWithoutCache(t *testing.T) {
	db := newFakeDB("1.11")
	db.add(1, true, 2)
	var s *Sampler
	for i := 0; i < 2; i++ {
		if got, known, err := s.Statements(context.Background(), db); err != nil || !known ||
			got[skey(1)].Calls != 2 {
			t.Fatalf("nil sampler: %v known=%v err=%v", got, known, err)
		}
	}
	if db.count("texts") != 2 {
		t.Fatalf("nil sampler cached (%d text reads)", db.count("texts"))
	}
}

// Concurrent access: samples from several goroutines share one cache.
func TestSampler_ConcurrentSamples(t *testing.T) {
	db := newFakeDB("1.11")
	db.add(1, true, 2)
	db.add(2, false, 2)
	s := NewSampler(nil)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				got, known, err := s.Statements(context.Background(), db)
				if err != nil || !known || len(got) != 1 || got[skey(1)].Calls != 2 {
					errs <- errors.New("concurrent sample wrong")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if db.count("texts") != 1 {
		t.Fatalf("concurrent samples read texts %d times, want once", db.count("texts"))
	}
}
