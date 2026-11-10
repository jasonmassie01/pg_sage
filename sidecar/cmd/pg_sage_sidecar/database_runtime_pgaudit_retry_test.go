package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/pgaudit"
)

// No concurrent access tests: one loop goroutine owns its retry state.

// flakyIngester fails its first failures calls, then stores what it gets.
type flakyIngester struct {
	failures int
	pending  int
	calls    int
	stored   map[string]int
	lastLen  int
}

func (f *flakyIngester) Ingest(_ context.Context, e []logwatch.LogEntry) (pgaudit.Stats,
	error) {
	f.calls++
	f.lastLen = len(e)
	if f.failures > 0 {
		f.failures--
		return pgaudit.Stats{}, errors.New("store down")
	}
	if f.stored == nil {
		f.stored = map[string]int{}
	}
	for _, x := range e {
		f.stored[x.Message]++
	}
	f.pending = 0
	return pgaudit.Stats{Stored: len(e), Dropped: 1, Unmatched: 2, Overflow: 3}, nil
}

func (f *flakyIngester) Pending() int { return f.pending }

// onceDrainer hands out its batch on the first drain and reports evictions.
type onceDrainer struct {
	batch   []logwatch.LogEntry
	evicted int
}

func (d *onceDrainer) Drain() []logwatch.LogEntry {
	b := d.batch
	d.batch = nil
	return b
}

func (d *onceDrainer) Stop() {}

func (d *onceDrainer) TakeEvicted() int {
	n := d.evicted
	d.evicted = 0
	return n
}

func entriesOf(n int) []logwatch.LogEntry {
	out := make([]logwatch.LogEntry, n)
	for i := range out {
		out[i] = logwatch.LogEntry{Message: "AUDIT: " + strconv.Itoa(i)}
	}
	return out
}

func recordCount(database, outcome string) int64 {
	for _, c := range pgaudit.RecordCounts() {
		if c.Database == database && c.Outcome == outcome {
			return c.Count
		}
	}
	return 0
}

// Error propagation: a store that fails twice and then recovers stores
// each record exactly once, and the retries are counted as requeued.
func TestPGAuditLoopRetriesFailedBatch(t *testing.T) {
	db := "retry_recovers"
	ing := &flakyIngester{failures: 2}
	l := newPGAuditLoop(&onceDrainer{batch: entriesOf(3)}, ing, db, "test")
	for range 4 {
		l.tick(context.Background())
	}
	if ing.calls != 3 || len(ing.stored) != 3 {
		t.Fatalf("calls=%d stored=%v; want 3 calls storing 3 records", ing.calls, ing.stored)
	}
	for msg, n := range ing.stored {
		if n != 1 {
			t.Fatalf("record %q stored %d times", msg, n)
		}
	}
	if got := recordCount(db, "requeued"); got != 6 {
		t.Fatalf("requeued = %d, want 6 (3 records, twice)", got)
	}
	if recordCount(db, "stored") != 3 || recordCount(db, "dropped") != 3 ||
		recordCount(db, "unmatched") != 3 {
		t.Fatalf("counts = %+v", pgaudit.RecordCounts())
	}
}

// Error propagation: a fourth failure drops the batch, counts it, and it
// is never sent again.
func TestPGAuditLoopDropsAfterFourthFailure(t *testing.T) {
	db := "retry_gives_up"
	ing := &flakyIngester{failures: 100}
	l := newPGAuditLoop(&onceDrainer{batch: entriesOf(5)}, ing, db, "test")
	for range 6 {
		l.tick(context.Background())
	}
	if ing.calls != 4 {
		t.Fatalf("ingest calls = %d, want 4 (one try, three retries)", ing.calls)
	}
	if got := recordCount(db, "dropped"); got != 5 {
		t.Fatalf("dropped = %d, want 5", got)
	}
	if got := recordCount(db, "requeued"); got != 15 {
		t.Fatalf("requeued = %d, want 15", got)
	}
}

// Boundary: a failed batch above 10,000 entries keeps the newest 10,000
// and drops the rest at once.
func TestPGAuditLoopRetryCap(t *testing.T) {
	db := "retry_cap"
	ing := &flakyIngester{failures: 1}
	l := newPGAuditLoop(&onceDrainer{batch: entriesOf(10_003)}, ing, db, "test")
	l.tick(context.Background())
	if got := recordCount(db, "dropped"); got != 3 {
		t.Fatalf("dropped = %d, want 3 above the cap", got)
	}
	l.tick(context.Background())
	if ing.lastLen != 10_000 {
		t.Fatalf("retried %d entries, want 10000", ing.lastLen)
	}
}

// State transitions: evictions are counted; held records are retried on a
// tick with nothing new; a tick with nothing at all calls nothing.
func TestPGAuditLoopEvictionsAndHeldRecords(t *testing.T) {
	db := "retry_evicted"
	ing := &flakyIngester{pending: 1}
	l := newPGAuditLoop(&onceDrainer{evicted: 7}, ing, db, "test")
	l.tick(context.Background())
	if got := recordCount(db, "evicted"); got != 7 {
		t.Fatalf("evicted = %d, want 7", got)
	}
	if ing.calls != 1 || ing.lastLen != 0 {
		t.Fatalf("calls=%d len=%d; want one ingest of the held records", ing.calls,
			ing.lastLen)
	}
	l.tick(context.Background())
	if ing.calls != 1 {
		t.Fatalf("an idle tick ingested (calls=%d)", ing.calls)
	}
}

// CHECK-07: /metrics exposes the counter with database and outcome labels.
func TestWritePGAuditMetrics(t *testing.T) {
	pgaudit.CountRecords("metrics_db", pgaudit.OutcomeStored, 4)
	var b strings.Builder
	writePGAuditMetrics(&b, pgaudit.RecordCounts())
	out := b.String()
	for _, want := range []string{"# TYPE pg_sage_pgaudit_records_total counter",
		`pg_sage_pgaudit_records_total{database="metrics_db",outcome="stored"} 4`} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics output lacks %q:\n%s", want, out)
		}
	}
}
