package snapstore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/snapstore"))
}

// requireDB returns a pool on the package's fixture database with the sage
// schema bootstrapped and sage.snapshots emptied.
func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE sage.snapshots"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool, ctx
}

type storedRow struct {
	id     int64
	at     time.Time
	baseID *int64
	data   string // reconstructed through the accessor
}

func storedRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cat string) []storedRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id, collected_at, base_id, `+DataSQL("")+
		`::text FROM sage.snapshots WHERE category = $1 ORDER BY collected_at, id`, cat)
	if err != nil {
		t.Fatalf("query %s rows: %v", cat, err)
	}
	defer rows.Close()
	var out []storedRow
	for rows.Next() {
		var r storedRow
		var data *string
		if err := rows.Scan(&r.id, &r.at, &r.baseID, &data); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if data != nil {
			r.data = *data
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// canonical returns the jsonb text of doc as PostgreSQL normalizes it.
func canonical(t *testing.T, ctx context.Context, pool *pgxpool.Pool, doc []byte) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT $1::jsonb::text`, string(doc)).Scan(&s); err != nil {
		t.Fatalf("canonical: %v", err)
	}
	return s
}

// Integration: the second cycle of a catalog category is a delta row that
// references the first cycle's keyframe and reads back identically; the
// system category stays a plain full row.
func TestPersist_SecondCycleIsDeltaOnFirstKeyframe(t *testing.T) {
	pool, ctx := requireDB(t)
	w := NewWriter()
	first := list(idx("a", 1), idx("b", 2))
	second := list(idx("a", 1), idx("b", 3), idx("c", 0))
	for i, doc := range [][]byte{first, second} {
		rows := []Row{{Category: "indexes", Data: doc},
			{Category: "system", Data: []byte(fmt.Sprintf(`{"db_size_bytes":%d}`, i))}}
		if err := w.Persist(ctx, pool, t0.Add(time.Duration(i)*time.Minute), rows); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}
	got := storedRows(t, ctx, pool, "indexes")
	if len(got) != 2 || got[0].baseID != nil || got[1].baseID == nil ||
		*got[1].baseID != got[0].id {
		t.Fatalf("index rows = %+v, want keyframe then delta on it", got)
	}
	if got[0].data != canonical(t, ctx, pool, first) ||
		got[1].data != canonical(t, ctx, pool, second) {
		t.Fatalf("read back %q / %q", got[0].data, got[1].data)
	}
	sys := storedRows(t, ctx, pool, "system")
	if len(sys) != 2 || sys[0].baseID != nil || sys[1].baseID != nil ||
		sys[1].data != `{"db_size_bytes": 1}` {
		t.Fatalf("system rows = %+v, want two full rows", sys)
	}
}

// Error propagation and state: a failed transaction writes nothing and
// does not advance the base, so the next cycle is a keyframe again and the
// error names the failing category.
func TestPersist_FailedTransactionDoesNotAdvanceBase(t *testing.T) {
	pool, ctx := requireDB(t)
	w := NewWriter()
	doc := list(idx("a", 1))
	err := w.Persist(ctx, pool, t0, []Row{{Category: "indexes", Data: doc},
		{Category: "system", Data: []byte(`{not json`)}})
	if err == nil || !strings.Contains(err.Error(), "system") {
		t.Fatalf("err = %v, want a failure naming the system row", err)
	}
	if n := len(storedRows(t, ctx, pool, "indexes")); n != 0 {
		t.Fatalf("%d index rows survived the rolled-back transaction", n)
	}
	if len(w.bases) != 0 {
		t.Fatalf("bases = %v after a failed transaction, want none", w.bases)
	}
	if err := w.Persist(ctx, pool, t0.Add(time.Minute),
		[]Row{{Category: "indexes", Data: doc}}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	got := storedRows(t, ctx, pool, "indexes")
	if len(got) != 1 || got[0].baseID != nil {
		t.Fatalf("rows = %+v, want one keyframe", got)
	}
}

// Error propagation: a cancelled context fails Persist without writing.
func TestPersist_CancelledContext(t *testing.T) {
	pool, ctx := requireDB(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err := NewWriter().Persist(cancelled, pool, t0,
		[]Row{{Category: "indexes", Data: list(idx("a", 1))}})
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want context canceled", err)
	}
	if n := len(storedRows(t, ctx, pool, "indexes")); n != 0 {
		t.Fatalf("%d rows written with a cancelled context", n)
	}
}

// Concurrency: Persist calls on one Writer are serialized; every row reads
// back as written and every delta references an existing keyframe.
func TestPersist_ConcurrentCallsStayConsistent(t *testing.T) {
	pool, ctx := requireDB(t)
	w := NewWriter()
	const workers, perWorker = 6, 5
	want := map[time.Time]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				at := t0.Add(time.Duration(g*perWorker+i) * time.Second)
				doc := list(idx("a", g), idx("b", i), idx(fmt.Sprintf("w%d", g), i))
				mu.Lock()
				want[at] = string(doc)
				mu.Unlock()
				errs <- w.Persist(ctx, pool, at, []Row{{Category: "indexes", Data: doc}})
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("persist: %v", err)
		}
	}
	got := storedRows(t, ctx, pool, "indexes")
	if len(got) != workers*perWorker {
		t.Fatalf("%d rows, want %d", len(got), workers*perWorker)
	}
	deltas := 0
	for _, r := range got {
		if r.data != canonical(t, ctx, pool, []byte(want[r.at])) {
			t.Fatalf("row at %s = %s, want %s", r.at, r.data, want[r.at])
		}
		if r.baseID != nil {
			deltas++
		}
	}
	if deltas == 0 {
		t.Fatal("no delta rows written: dedupe never engaged")
	}
}
