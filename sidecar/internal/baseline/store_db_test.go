package baseline

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// market-02: sage.baseline_samples keeps one aggregated value per
// (database, metric, key, week, hour-of-week bucket). History reads the
// bucket's previous weeks, never the current one.

func liveStore(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.baseline_samples`); err != nil {
		t.Fatalf("clear baseline_samples: %v", err)
	}
	return NewStore(pool), pool, ctx
}

// monday9 is Monday 2026-11-09 09:00 UTC (bucket 9).
var monday9 = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

var queryKey = Key{Metric: "query_mean_ms", Key: "queryid:42"}

func sample(k Key, v float64) []Sample {
	return []Sample{{Key: k, How: "interval_mean", Value: v}}
}

// Happy path: three cycles in one hour fold into one row, n = 3, value
// the mean.
func TestBaselineStoreAccumulate_FoldsAnHourIntoOneRow(t *testing.T) {
	st, pool, ctx := liveStore(t)
	w := Window{Loc: time.UTC, Weeks: 4}
	for i, v := range []float64{10, 20, 60} {
		at := monday9.Add(time.Duration(i) * 10 * time.Minute)
		if err := st.Accumulate(ctx, "orders", w, at, sample(queryKey, v)); err != nil {
			t.Fatalf("accumulate %d: %v", i, err)
		}
	}
	var n int64
	var value float64
	var bucket int
	var week time.Time
	var how string
	if err := pool.QueryRow(ctx, `SELECT n, value, bucket, week_start, how
		FROM sage.baseline_samples WHERE database = 'orders'`).
		Scan(&n, &value, &bucket, &week, &how); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if n != 3 || math.Abs(value-30) > 1e-9 || bucket != 9 || how != "interval_mean" ||
		!week.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("row = n %d value %v bucket %d week %s how %q, want 3, 30, 9, "+
			"2026-11-09, interval_mean", n, value, bucket, week, how)
	}
}

// Boundary: History returns the bucket's previous `weeks` weeks, oldest
// first: not the current week, not an older week, not another bucket.
func TestBaselineStoreHistory_ReadsThePreviousWeeksOfTheBucket(t *testing.T) {
	st, _, ctx := liveStore(t)
	w := Window{Loc: time.UTC, Weeks: 4}
	week := 7 * 24 * time.Hour
	for i := 5; i >= 0; i-- { // weeks -5 .. 0
		at := monday9.Add(-time.Duration(i) * week)
		if err := st.Accumulate(ctx, "orders", w, at, sample(queryKey,
			float64(100-i))); err != nil {
			t.Fatalf("seed week -%d: %v", i, err)
		}
	}
	// Another bucket (Tuesday 09:00) of last week.
	if err := st.Accumulate(ctx, "orders", w, monday9.Add(-week+24*time.Hour),
		sample(queryKey, 1)); err != nil {
		t.Fatalf("seed tuesday: %v", err)
	}
	got, err := st.History(ctx, "orders", w, monday9.Add(15*time.Minute), []Key{queryKey})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	want := []float64{96, 97, 98, 99}
	if h := got[queryKey]; len(h) != len(want) {
		t.Fatalf("history = %v, want %v", h, want)
	}
	for i, v := range want {
		if got[queryKey][i] != v {
			t.Fatalf("history = %v, want %v", got[queryKey], want)
		}
	}
	unknown := Key{Metric: "query_mean_ms", Key: "queryid:7"}
	got, err = st.History(ctx, "orders", w, monday9, []Key{unknown})
	if err != nil || len(got[unknown]) != 0 {
		t.Fatalf("unknown key = %v (%v), want no history", got, err)
	}
}

// Integration (fleet): two databases keep separate baselines for the
// same key.
func TestBaselineStoreHistory_FleetDatabasesAreSeparate(t *testing.T) {
	st, _, ctx := liveStore(t)
	w := Window{Loc: time.UTC, Weeks: 4}
	for i := 1; i <= 3; i++ {
		at := monday9.Add(-time.Duration(i) * 7 * 24 * time.Hour)
		if err := st.Accumulate(ctx, "orders", w, at, sample(queryKey, 5)); err != nil {
			t.Fatalf("orders: %v", err)
		}
		if err := st.Accumulate(ctx, "billing", w, at, sample(queryKey, 80)); err != nil {
			t.Fatalf("billing: %v", err)
		}
	}
	orders, err := st.History(ctx, "orders", w, monday9, []Key{queryKey})
	if err != nil {
		t.Fatalf("orders history: %v", err)
	}
	billing, err := st.History(ctx, "billing", w, monday9, []Key{queryKey})
	if err != nil {
		t.Fatalf("billing history: %v", err)
	}
	if len(orders[queryKey]) != 3 || orders[queryKey][0] != 5 ||
		len(billing[queryKey]) != 3 || billing[queryKey][0] != 80 {
		t.Fatalf("orders %v billing %v, want 3 weeks of 5 and of 80",
			orders[queryKey], billing[queryKey])
	}
}

// Concurrent access: accumulation from several goroutines while History
// reads loses no sample: n is the number of writes and the value their
// mean.
func TestBaselineStoreAccumulate_ConcurrentWritersAndReaders(t *testing.T) {
	st, pool, ctx := liveStore(t)
	w := Window{Loc: time.UTC, Weeks: 4}
	const writers, each = 6, 20
	var wg sync.WaitGroup
	errs := make(chan error, writers*each+writers)
	for g := 0; g < writers; g++ {
		wg.Add(2)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				errs <- st.Accumulate(ctx, "orders", w, monday9, sample(queryKey,
					float64(g+1)))
			}
		}(g)
		go func() {
			defer wg.Done()
			_, err := st.History(ctx, "orders", w, monday9, []Key{queryKey})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent call: %v", err)
		}
	}
	var n int64
	var value float64
	if err := pool.QueryRow(ctx, `SELECT n, value FROM sage.baseline_samples
		WHERE database = 'orders'`).Scan(&n, &value); err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != writers*each || math.Abs(value-3.5) > 1e-9 { // mean of 1..6
		t.Fatalf("n %d value %v, want %d and 3.5", n, value, writers*each)
	}
}

// Nil/empty: no samples writes nothing; no keys reads nothing.
func TestBaselineStore_EmptyInputsAreNoOps(t *testing.T) {
	st, pool, ctx := liveStore(t)
	w := Window{Loc: time.UTC, Weeks: 4}
	if err := st.Accumulate(ctx, "orders", w, monday9, nil); err != nil {
		t.Fatalf("empty accumulate: %v", err)
	}
	got, err := st.History(ctx, "orders", w, monday9, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty history = %v (%v), want none", got, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.baseline_samples`).
		Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d (%v), want 0", n, err)
	}
	// Invalid values are dropped before the write.
	if err := st.Accumulate(ctx, "orders", w, monday9, sample(queryKey,
		math.NaN())); err != nil {
		t.Fatalf("NaN accumulate: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.baseline_samples`).
		Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after NaN = %d (%v), want 0", n, err)
	}
}

// Error propagation: a store without a pool and a database without the
// sage schema return errors that name what failed.
func TestBaselineStore_ErrorsNameTheTable(t *testing.T) {
	ctx := context.Background()
	w := Window{Loc: time.UTC, Weeks: 4}
	var nilStore *Store
	if err := nilStore.Accumulate(ctx, "x", w, monday9, sample(queryKey, 1)); err == nil {
		t.Fatal("nil store accumulated")
	}
	if _, err := NewStore(nil).History(ctx, "x", w, monday9, []Key{queryKey}); err == nil {
		t.Fatal("store without a pool read history")
	}
	fresh, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "baseline_fresh"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(fresh.Close)
	err = NewStore(fresh).Accumulate(ctx, "x", w, monday9, sample(queryKey, 1))
	if err == nil || !strings.Contains(err.Error(), "baseline_samples") {
		t.Fatalf("accumulate without schema = %v, want an error naming baseline_samples", err)
	}
	_, err = NewStore(fresh).History(ctx, "x", w, monday9, []Key{queryKey})
	if err == nil || !strings.Contains(err.Error(), "baseline_samples") {
		t.Fatalf("history without schema = %v, want an error naming baseline_samples", err)
	}
}
