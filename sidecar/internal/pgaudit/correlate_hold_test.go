package pgaudit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

var errFakeStore = errors.New("store unavailable")

// countingQuerier counts every round trip. Each action lookup answers
// actionID (0: no row); the first execErrs stores fail.
type countingQuerier struct {
	mu       sync.Mutex
	queries  int
	execs    int
	actionID int64
	execErrs int
	bodies   []string
}

func (q *countingQuerier) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries++
	return fakeRow{id: q.actionID}
}

func (q *countingQuerier) Exec(_ context.Context, _ string, args ...any) (
	pgconn.CommandTag, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.execs++
	if q.execErrs > 0 {
		q.execErrs--
		return pgconn.CommandTag{}, errFakeStore
	}
	q.bodies = append(q.bodies, args[0].(string))
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (q *countingQuerier) setAction(id int64) {
	q.mu.Lock()
	q.actionID = id
	q.mu.Unlock()
}

func (q *countingQuerier) counts() (int, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queries, q.execs
}

type fakeRow struct{ id int64 }

func (r fakeRow) Scan(dest ...any) error {
	if r.id == 0 {
		return pgx.ErrNoRows
	}
	*(dest[0].(*int64)) = r.id
	return nil
}

// fakeCorrelator has its own role read already and a settable clock.
func fakeCorrelator(q *countingQuerier, now *time.Time) *Correlator {
	c := newCorrelator(q, "orders")
	c.ownRole = ownRole
	c.now = func() time.Time { return *now }
	return c
}

const untaggedDDL = `AUDIT: SESSION,1,1,DDL,CREATE INDEX,INDEX,public.i,` +
	`CREATE INDEX CONCURRENTLY i ON public.t (a)`

// Nil/empty and performance: pg_sage's own READ records are dropped before
// any lookup, and an empty ingest with nothing held costs no round trip.
func TestIngestSkipsPGSageReadsWithoutQueries(t *testing.T) {
	now := time.Now()
	q := &countingQuerier{}
	c := fakeCorrelator(q, &now)
	entries := make([]logwatch.LogEntry, 1000)
	for i := range entries {
		entries[i] = entry("pg_sage", ownRole,
			`AUDIT: SESSION,1,1,READ,SELECT,,,SELECT * FROM pg_stat_statements`)
	}
	st, err := c.Ingest(context.Background(), entries)
	if err != nil || st.Dropped != 1000 || st.Stored != 0 {
		t.Fatalf("ingest = %+v, %v; want 1000 dropped", st, err)
	}
	if queries, execs := q.counts(); queries != 0 || execs != 0 {
		t.Fatalf("1,000 READ records cost %d queries and %d stores, want none", queries,
			execs)
	}
	if st, err := c.Ingest(context.Background(), nil); err != nil || st != (Stats{}) {
		t.Fatalf("empty ingest = %+v, %v", st, err)
	}
	if queries, _ := q.counts(); queries != 0 {
		t.Fatalf("empty ingest ran %d queries", queries)
	}
}

// A batch of tagged records looks each decision up once.
func TestIngestTaggedLooksUpEachDecisionOnce(t *testing.T) {
	now := time.Now()
	q := &countingQuerier{actionID: 5}
	c := fakeCorrelator(q, &now)
	st, err := c.Ingest(context.Background(), []logwatch.LogEntry{
		entry("pg_sage action:7", ownRole, untaggedDDL),
		entry("pg_sage action:7", ownRole, `AUDIT: SESSION,2,1,MISC,SET,,,SET x = 1`),
		entry("pg_sage action:7", ownRole, untaggedDDL),
		entry("pg_sage action:8", ownRole, untaggedDDL)})
	if err != nil || st.Stored != 4 {
		t.Fatalf("ingest = %+v, %v; want 4 stored", st, err)
	}
	if queries, execs := q.counts(); queries != 2 || execs != 1 {
		t.Fatalf("queries=%d stores=%d, want one lookup per decision and one store",
			queries, execs)
	}
	if !strings.Contains(q.bodies[0], `"correlated_by":"action_tag"`) ||
		!strings.Contains(q.bodies[0], `"action_id":5`) {
		t.Fatalf("stored rows = %s", q.bodies[0])
	}
}

// Boundary: an untagged pg_sage DDL record is held; matched 4 minutes on it
// is stored; held exactly 5 minutes it is still retried; 5 minutes and one
// second after it was first seen it is dropped as unmatched, unqueried.
func TestIngestHoldsUntaggedRecordFiveMinutes(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	now := start
	q := &countingQuerier{}
	c := fakeCorrelator(q, &now)
	st, err := c.Ingest(ctx, []logwatch.LogEntry{entry("pg_sage", ownRole, untaggedDDL)})
	if err != nil || st.Held != 1 || st.Stored != 0 || c.Pending() != 1 {
		t.Fatalf("first ingest = %+v, %v pending=%d; want 1 held", st, err, c.Pending())
	}
	now = start.Add(4 * time.Minute)
	q.setAction(11)
	if st, err := c.Ingest(ctx, nil); err != nil || st.Stored != 1 || c.Pending() != 0 {
		t.Fatalf("ingest 4 minutes on = %+v, %v; want the held record stored", st, err)
	}
	if !strings.Contains(q.bodies[0], `"correlated_by":"statement"`) ||
		!strings.Contains(q.bodies[0], `"action_id":11`) {
		t.Fatalf("stored rows = %s", q.bodies[0])
	}

	q.setAction(0)
	now = start
	if _, err := c.Ingest(ctx, []logwatch.LogEntry{entry("pg_sage", ownRole,
		untaggedDDL)}); err != nil {
		t.Fatalf("hold: %v", err)
	}
	now = start.Add(5 * time.Minute)
	if st, err := c.Ingest(ctx, nil); err != nil || st.Held != 1 || st.Unmatched != 0 {
		t.Fatalf("ingest at exactly 5 minutes = %+v, %v; want still held", st, err)
	}
	queries, _ := q.counts()
	now = start.Add(5*time.Minute + time.Second)
	st, err = c.Ingest(ctx, nil)
	if err != nil || st.Unmatched != 1 || st.Held != 0 || c.Pending() != 0 {
		t.Fatalf("ingest at 5m1s = %+v, %v; want dropped as unmatched", st, err)
	}
	if after, _ := q.counts(); after != queries {
		t.Fatalf("an expired record was looked up again (%d queries)", after-queries)
	}
}

// Boundary: at most 1,000 records are held; the oldest beyond that are
// dropped and counted, and non-DDL statements are never held.
func TestIngestHoldCap(t *testing.T) {
	now := time.Now()
	q := &countingQuerier{}
	c := fakeCorrelator(q, &now)
	entries := make([]logwatch.LogEntry, 1001)
	for i := range entries {
		entries[i] = entry("pg_sage", ownRole, untaggedDDL)
	}
	entries = append(entries, entry("pg_sage", ownRole,
		`AUDIT: SESSION,1,1,WRITE,INSERT,,,INSERT INTO sage.snapshots VALUES (1)`))
	st, err := c.Ingest(context.Background(), entries)
	if err != nil || st.Held != 1000 || st.Overflow != 1 || st.Dropped != 1 {
		t.Fatalf("ingest = %+v, %v; want 1000 held, 1 overflow, 1 dropped", st, err)
	}
	if c.Pending() != 1000 {
		t.Fatalf("pending = %d, want 1000", c.Pending())
	}
}

// Error propagation and state transition: a failed store leaves the held
// records as they were and stores nothing, so a retried batch is neither
// lost nor held twice.
func TestIngestStoreFailureKeepsHeldState(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	q := &countingQuerier{}
	c := fakeCorrelator(q, &now)
	if _, err := c.Ingest(ctx, []logwatch.LogEntry{entry("pg_sage", ownRole,
		untaggedDDL)}); err != nil || c.Pending() != 1 {
		t.Fatalf("hold: %v pending=%d", err, c.Pending())
	}
	q.execErrs = 1
	batch := []logwatch.LogEntry{entry("pg_sage action:3", ownRole, untaggedDDL),
		entry("pg_sage", ownRole, untaggedDDL)}
	if _, err := c.Ingest(ctx, batch); !errors.Is(err, errFakeStore) {
		t.Fatalf("failing store: err = %v", err)
	}
	if c.Pending() != 1 {
		t.Fatalf("pending after a failed store = %d, want the 1 held before", c.Pending())
	}
	st, err := c.Ingest(ctx, batch)
	if err != nil || st.Stored != 1 || c.Pending() != 2 {
		t.Fatalf("retried batch = %+v, %v pending=%d; want 1 stored, 2 held", st, err,
			c.Pending())
	}
	q.setAction(9)
	if st, err := c.Ingest(ctx, nil); err != nil || st.Stored != 2 || c.Pending() != 0 {
		t.Fatalf("matched later = %+v, %v; want both held stored once", st, err)
	}
}

// Concurrent access: ingests from several goroutines hold each record once
// and store each once when its action appears.
func TestIngestConcurrentHolds(t *testing.T) {
	now := time.Now()
	q := &countingQuerier{}
	c := fakeCorrelator(q, &now)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Ingest(context.Background(), []logwatch.LogEntry{
				entry("pg_sage", ownRole, untaggedDDL),
				entry("pg_sage action:4", ownRole, untaggedDDL)})
		}()
	}
	wg.Wait()
	if c.Pending() != 8 {
		t.Fatalf("pending = %d, want 8", c.Pending())
	}
	q.setAction(2)
	st, err := c.Ingest(context.Background(), nil)
	if err != nil || st.Stored != 8 || c.Pending() != 0 {
		t.Fatalf("drain = %+v, %v; want the 8 held stored", st, err)
	}
	if got := strings.Count(strings.Join(q.bodies, ""), `"correlated_by":"`); got != 16 {
		t.Fatalf("stored %d records in all, want 16 (8 tagged, 8 matched)", got)
	}
}
