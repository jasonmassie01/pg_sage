package schema

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// config-31: PersistTrustRampStart on a real server, pinned before it was
// split into readTrustRampStart and initTrustRampStart.

// freshTrustRampStart bootstraps sage and removes the trust_ramp_start row
// now and when the test ends.
func freshTrustRampStart(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	clear := func() {
		if _, err := pool.Exec(context.Background(),
			"DELETE FROM sage.config WHERE key = 'trust_ramp_start'"); err != nil {
			t.Errorf("clear trust_ramp_start: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
	return pool, ctx
}

type trustRampRow struct {
	count     int
	value     string
	updatedBy string
}

func readTrustRampRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) trustRampRow {
	t.Helper()
	var r trustRampRow
	err := pool.QueryRow(ctx, `SELECT count(*), coalesce(max(value), ''),
		coalesce(max(updated_by), '') FROM sage.config
		WHERE key = 'trust_ramp_start'`).Scan(&r.count, &r.value, &r.updatedBy)
	if err != nil {
		t.Fatalf("read trust_ramp_start rows: %v", err)
	}
	return r
}

func storeTrustRampStart(t *testing.T, ctx context.Context, pool *pgxpool.Pool, v string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.config (key, value, updated_by)
		VALUES ('trust_ramp_start', $1, 'test')`, v); err != nil {
		t.Fatalf("store trust_ramp_start %q: %v", v, err)
	}
}

// Nil/empty: a zero config value and no row take the database's now() and
// write exactly one row, tagged bootstrap, that reads back as the result.
func TestPersistTrustRampStart_ZeroConfigWritesOneRow(t *testing.T) {
	pool, ctx := freshTrustRampStart(t)
	before := databaseNow(t, ctx, pool)
	got, err := PersistTrustRampStart(ctx, pool, time.Time{})
	if err != nil {
		t.Fatalf("PersistTrustRampStart(zero): %v", err)
	}
	after := databaseNow(t, ctx, pool)
	if got.Before(before) || got.After(after) {
		t.Fatalf("got %v, want the database's now() in [%v, %v]", got, before, after)
	}
	row := readTrustRampRows(t, ctx, pool)
	if row.count != 1 || row.updatedBy != "bootstrap" {
		t.Fatalf("rows = %+v, want one row updated_by bootstrap", row)
	}
	again, err := PersistTrustRampStart(ctx, pool, time.Time{})
	if err != nil || !again.Equal(got) {
		t.Fatalf("second call = %v, %v; want the stored %v", again, err, got)
	}
}

// State transition: a stored row wins over the config value, and is left
// untouched.
func TestPersistTrustRampStart_StoredRowWinsOverConfig(t *testing.T) {
	pool, ctx := freshTrustRampStart(t)
	storeTrustRampStart(t, ctx, pool, "2025-03-04T05:06:07.123456+00")
	cfg := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := PersistTrustRampStart(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("PersistTrustRampStart: %v", err)
	}
	want := time.Date(2025, 3, 4, 5, 6, 7, 123456000, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v, want the stored %v", got, want)
	}
	row := readTrustRampRows(t, ctx, pool)
	if row.count != 1 || row.updatedBy != "test" ||
		row.value != "2025-03-04T05:06:07.123456+00" {
		t.Fatalf("stored row changed: %+v", row)
	}
}

// Boundary: a config value with nanoseconds and a non-UTC zone is stored at
// PostgreSQL's microsecond precision as the same instant.
func TestPersistTrustRampStart_ConfigValueKeepsTheInstantToTheMicrosecond(t *testing.T) {
	pool, ctx := freshTrustRampStart(t)
	zone := time.FixedZone("UTC-5", -5*3600)
	cfg := time.Date(2025, 1, 1, 7, 0, 0, 123456400, zone)
	got, err := PersistTrustRampStart(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("PersistTrustRampStart: %v", err)
	}
	if got.Nanosecond()%1000 != 0 {
		t.Fatalf("got %v, want microsecond precision", got)
	}
	if d := got.Sub(cfg); d < -time.Microsecond || d > time.Microsecond {
		t.Fatalf("got %v, want %v to the microsecond (off by %v)", got, cfg, d)
	}
	if row := readTrustRampRows(t, ctx, pool); row.count != 1 || row.updatedBy != "bootstrap" {
		t.Fatalf("rows = %+v, want one bootstrap row", row)
	}
}

// Invalid input: a stored value that does not parse is an error naming the
// value, and the row is not replaced.
func TestPersistTrustRampStart_UnparseableStoredValueFails(t *testing.T) {
	pool, ctx := freshTrustRampStart(t)
	storeTrustRampStart(t, ctx, pool, "not-a-date")
	got, err := PersistTrustRampStart(ctx, pool, time.Now())
	if err == nil {
		t.Fatalf("PersistTrustRampStart = %v, want a parse error", got)
	}
	want := `parsing trust_ramp_start "not-a-date": no matching format`
	if err.Error() != want || !got.IsZero() {
		t.Fatalf("got %v, %q; want zero time and %q", got, err, want)
	}
	if row := readTrustRampRows(t, ctx, pool); row.count != 1 || row.value != "not-a-date" {
		t.Fatalf("stored row changed: %+v", row)
	}
}

// Error propagation: a cancelled context is an error wrapping
// context.Canceled, with no row written.
func TestPersistTrustRampStart_CancelledContextWrapsCanceled(t *testing.T) {
	pool, ctx := freshTrustRampStart(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	got, err := PersistTrustRampStart(cctx, pool, time.Time{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want one wrapping context.Canceled", err)
	}
	if !got.IsZero() || !strings.Contains(err.Error(), "trust_ramp_start") {
		t.Fatalf("got %v, %q; want zero time and an error naming trust_ramp_start",
			got, err)
	}
	if row := readTrustRampRows(t, ctx, pool); row.count != 0 {
		t.Fatalf("a cancelled call wrote %+v", row)
	}
}

// Concurrency: eight instances booting at once on a database with no row
// agree on one start time, and only one row exists (the race path).
func TestPersistTrustRampStart_ConcurrentFirstBootsAgree(t *testing.T) {
	pool, ctx := freshTrustRampStart(t)
	const n = 8
	results := make([]time.Time, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = PersistTrustRampStart(ctx, pool, time.Time{})
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i].IsZero() || !results[i].Equal(results[0]) {
			t.Fatalf("caller %d got %v, caller 0 got %v: callers disagree",
				i, results[i], results[0])
		}
	}
	if row := readTrustRampRows(t, ctx, pool); row.count != 1 {
		t.Fatalf("%d trust_ramp_start rows after %d concurrent boots, want 1",
			row.count, n)
	}
}

// The split kept the SQL byte for byte: these are the literals the unsplit
// PersistTrustRampStart sent.
func TestTrustRampStartSQL_IsUnchangedBySplit(t *testing.T) {
	cases := map[string][2]string{
		"select": {selectTrustRampStartSQL,
			"SELECT value FROM sage.config WHERE key = 'trust_ramp_start'"},
		"insert": {insertTrustRampStartSQL,
			"INSERT INTO sage.config (key, value, updated_by) VALUES " +
				"('trust_ramp_start', $1, 'bootstrap') RETURNING value::timestamptz"},
		"insert now": {insertTrustRampStartNowSQL,
			"INSERT INTO sage.config (key, value, updated_by) VALUES ('trust_ramp_start', " +
				"to_char(now(), 'YYYY-MM-DD\"T\"HH24:MI:SS.USOF'), 'bootstrap') " +
				"RETURNING value::timestamptz"},
		"reread": {rereadTrustRampStartSQL,
			"SELECT value::timestamptz FROM sage.config WHERE key = 'trust_ramp_start'"},
	}
	for name, c := range cases {
		if c[0] != c[1] {
			t.Errorf("%s SQL = %q, want %q", name, c[0], c[1])
		}
	}
	if len(trustRampStartLayouts) != 5 || trustRampStartLayouts[0] != time.RFC3339Nano {
		t.Errorf("layouts = %q, want the five unsplit layouts", trustRampStartLayouts)
	}
}
