package partition

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func requireDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("SAGE_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_DATABASE_URL not set: partition integration tests need PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS sage"); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return pool, ctx
}

// scratch creates sage.<name> shaped like a history table: a bigserial
// key, a time column, a value, three secondary indexes and a grant.
func scratch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, key []string) Table {
	t.Helper()
	var b [4]byte
	_, _ = rand.Read(b[:])
	tbl := Table{Name: fmt.Sprintf("pt_%x", b), Column: "at", Key: key}
	exec(t, ctx, pool, fmt.Sprintf(`CREATE TABLE sage.%[1]s (
		id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now(), v int NOT NULL);
		CREATE INDEX %[1]s_v_at ON sage.%[1]s (v, at DESC);
		CREATE INDEX %[1]s_at ON sage.%[1]s (at DESC);
		CREATE INDEX %[1]s_big ON sage.%[1]s (v) WHERE v > 100;
		GRANT SELECT ON sage.%[1]s TO PUBLIC`, tbl.Name))
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DROP TABLE IF EXISTS sage."+tbl.Name+" CASCADE")
		_, _ = pool.Exec(bg, "DROP TABLE IF EXISTS sage."+tbl.HistoryName())
	})
	return tbl
}

func exec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %s: %v", sql, err)
	}
}

func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string,
	args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query %s: %v", sql, err)
	}
	return n
}

func relkind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rel string) string {
	t.Helper()
	var k string
	if err := pool.QueryRow(ctx, `SELECT relkind::text FROM pg_class
		WHERE oid = to_regclass($1)`, rel).Scan(&k); err != nil {
		t.Fatalf("relkind of %s: %v", rel, err)
	}
	return k
}

func indexNames(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT indexname FROM pg_indexes
		WHERE schemaname = 'sage' AND tablename = $1 ORDER BY 1`, table)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(names, ",")
}

func converted(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tbl Table) {
	t.Helper()
	if ok, err := Convert(ctx, pool, tbl); err != nil || !ok {
		t.Fatalf("Convert(%s) = %v, %v", tbl.Name, ok, err)
	}
}

func TestConvert_PlainTableBecomesPartitionedWithHistory(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	exec(t, ctx, pool, `INSERT INTO sage.`+tbl.Name+` (at, v)
		SELECT now() - g * interval '1 day', g FROM generate_series(0, 199) g`)
	maxID := count(t, ctx, pool, "SELECT max(id) FROM sage."+tbl.Name)

	converted(t, ctx, pool, tbl)
	if k := relkind(t, ctx, pool, "sage."+tbl.Name); k != "p" {
		t.Fatalf("relkind = %q, want partitioned", k)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM ONLY sage."+tbl.HistoryName()); n != 200 {
		t.Fatalf("history partition holds %d rows, want all 200", n)
	}
	// The parent has every secondary index under its original name; the
	// single-column key is gone (Key is nil: nothing looks rows up by id).
	want := tbl.Name + "_at," + tbl.Name + "_big," + tbl.Name + "_v_at"
	if got := indexNames(t, ctx, pool, tbl.Name); got != want {
		t.Fatalf("parent indexes = %s, want %s", got, want)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = to_regclass($1) AND contype = 'p'`,
		"sage."+tbl.HistoryName()); n != 0 {
		t.Fatal("history kept its primary key")
	}
	// New rows go through the parent with ids from the same sequence.
	var id int64
	if err := pool.QueryRow(ctx, "INSERT INTO sage."+tbl.Name+" (v) VALUES (7) RETURNING id").
		Scan(&id); err != nil {
		t.Fatalf("insert after convert: %v", err)
	}
	if id <= maxID {
		t.Fatalf("new id %d does not follow %d", id, maxID)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM aclexplode((SELECT relacl FROM pg_class
		WHERE oid = to_regclass($1))) WHERE grantee = 0 AND privilege_type = 'SELECT'`,
		"sage."+tbl.Name); n != 1 {
		t.Fatal("PUBLIC lost SELECT on the converted table")
	}
	// Dropping the history partition must not drop the shared sequence.
	exec(t, ctx, pool, "DROP TABLE sage."+tbl.HistoryName())
	exec(t, ctx, pool, "INSERT INTO sage."+tbl.Name+" (v) VALUES (8)")
}

func TestConvert_IsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	converted(t, ctx, pool, tbl)
	before := indexNames(t, ctx, pool, tbl.Name)
	ok, err := Convert(ctx, pool, tbl)
	if err != nil || ok {
		t.Fatalf("second Convert = %v, %v; want a no-op", ok, err)
	}
	if got := indexNames(t, ctx, pool, tbl.Name); got != before {
		t.Fatalf("indexes changed on a no-op convert: %s -> %s", before, got)
	}
}

func TestConvert_MissingTableIsAnError(t *testing.T) {
	pool, ctx := requireDB(t)
	_, err := Convert(ctx, pool, Table{Name: "no_such_history", Column: "at"})
	if err == nil || !strings.Contains(err.Error(), "no_such_history") {
		t.Fatalf("Convert(missing) = %v, want an error naming the table", err)
	}
}

func TestConvert_KeyBecomesCompositePrimaryKey(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, []string{"id"})
	exec(t, ctx, pool, "INSERT INTO sage."+tbl.Name+" (v) VALUES (1), (2)")
	converted(t, ctx, pool, tbl)
	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = to_regclass($1) AND contype = 'p'`, "sage."+tbl.Name).
		Scan(&def); err != nil {
		t.Fatalf("primary key: %v", err)
	}
	if def != "PRIMARY KEY (id, at)" {
		t.Fatalf("primary key = %q", def)
	}
	// A duplicate (id, at) is still refused.
	_, err := pool.Exec(ctx, "INSERT INTO sage."+tbl.Name+" (id, at, v) SELECT id, at, 9 FROM sage."+
		tbl.Name+" LIMIT 1")
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate key accepted: %v", err)
	}
}

// History ends tomorrow at 00:00 UTC. A row dated later (a skewed clock, a
// test fixture in 2099) waits in the DEFAULT partition and moves into its
// day when that day's partition is created; nothing is refused.
func TestConvert_RowsDatedAheadWaitInDefault(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	now := time.Now()
	ahead := DayStart(now).Add(2*24*time.Hour + time.Hour)
	far := time.Date(2099, 7, 22, 12, 0, 0, 0, time.UTC)
	exec(t, ctx, pool, "INSERT INTO sage."+tbl.Name+" (at, v) VALUES ($1, 1), ($2, 2), (now(), 3)",
		ahead, far)
	converted(t, ctx, pool, tbl)
	parts, err := List(ctx, pool, tbl)
	if err != nil || len(parts) != 2 || !parts[0].History || !parts[1].Default {
		t.Fatalf("List = %+v, %v; want history and default", parts, err)
	}
	if want := DayStart(now).Add(24 * time.Hour); !parts[0].Upper.Equal(want) {
		t.Fatalf("history upper = %s, want %s", parts[0].Upper, want)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM ONLY sage."+tbl.DefaultName()); n != 2 {
		t.Fatalf("default holds %d rows, want the two dated ahead", n)
	}
	if _, err := Ensure(ctx, pool, tbl, now, 3); err != nil {
		t.Fatalf("Ensure over a default partition holding a row of the day: %v", err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM ONLY sage."+tbl.DayName(ahead)); n != 1 {
		t.Fatalf("%s holds %d rows, want the moved one", tbl.DayName(ahead), n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM ONLY sage."+tbl.DefaultName()); n != 1 {
		t.Fatalf("default holds %d rows after the move, want the 2099 one", n)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sage."+tbl.Name); n != 3 {
		t.Fatalf("%d rows in all, want 3", n)
	}
}

// days returns the daily partitions of a listing.
func days(parts []Partition) []Partition {
	var out []Partition
	for _, p := range parts {
		if !p.History && !p.Default {
			out = append(out, p)
		}
	}
	return out
}

func TestEnsure_CreatesMissingDaysAfterHistoryOnly(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	if n, err := Ensure(ctx, pool, tbl, time.Now(), 3); err != nil || n != 0 {
		t.Fatalf("Ensure on a plain table = %d, %v; want a no-op", n, err)
	}
	converted(t, ctx, pool, tbl)
	now := time.Now()
	// History ends tomorrow at 00:00 UTC, so of [today, today+4) only the
	// days from tomorrow on need partitions.
	n, err := Ensure(ctx, pool, tbl, now, 4)
	if err != nil || n != 3 {
		t.Fatalf("Ensure = %d, %v; want 3 new daily partitions", n, err)
	}
	if n, err := Ensure(ctx, pool, tbl, now, 4); err != nil || n != 0 {
		t.Fatalf("repeat Ensure = %d, %v; want 0", n, err)
	}
	parts, err := List(ctx, pool, tbl)
	if err != nil || len(parts) != 5 || !parts[0].History || !parts[4].Default {
		t.Fatalf("List = %+v, %v; want history, 3 days, default", parts, err)
	}
	for i, p := range days(parts) {
		day := DayStart(now).Add(time.Duration(i+1) * 24 * time.Hour)
		if p.History || p.Name != tbl.DayName(day) || !p.Lower.Equal(day) ||
			!p.Upper.Equal(day.Add(24*time.Hour)) {
			t.Fatalf("partition %d = %+v, want day %s", i+1, p, day)
		}
	}
	// A row two days ahead lands in its day, which has the parent's indexes.
	exec(t, ctx, pool, "INSERT INTO sage."+tbl.Name+" (at, v) VALUES ($1, 3)",
		DayStart(now).Add(48*time.Hour+time.Minute))
	day2 := tbl.DayName(DayStart(now).Add(48 * time.Hour))
	if n := count(t, ctx, pool, "SELECT count(*) FROM ONLY sage."+day2); n != 1 {
		t.Fatalf("%s holds %d rows", day2, n)
	}
	if got := indexNames(t, ctx, pool, day2); strings.Count(got, ",") != 2 {
		t.Fatalf("%s indexes = %s, want the parent's three", day2, got)
	}
	if _, err := Ensure(ctx, pool, tbl, now, 0); err == nil {
		t.Fatal("Ensure accepted zero days")
	}
}

func TestEnsure_ConcurrentCallersAgree(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	converted(t, ctx, pool, tbl)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	created := make(chan int, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := Ensure(ctx, pool, tbl, time.Now(), 5)
			errs <- err
			created <- n
		}()
	}
	wg.Wait()
	close(errs)
	close(created)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Ensure: %v", err)
		}
	}
	total := 0
	for n := range created {
		total += n
	}
	parts, err := List(ctx, pool, tbl)
	if err != nil || len(days(parts)) != 4 || total != 4 {
		t.Fatalf("partitions = %+v (%v), created in all = %d; want 4 days, 4",
			parts, err, total)
	}
}

func TestDropAndTruncate(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	exec(t, ctx, pool, "INSERT INTO sage."+tbl.Name+
		" (at, v) VALUES (now() - interval '9 days', 1)")
	converted(t, ctx, pool, tbl)
	if _, err := Ensure(ctx, pool, tbl, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	parts, _ := List(ctx, pool, tbl)
	last := days(parts)[len(days(parts))-1]
	if err := Drop(ctx, pool, tbl, last); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM pg_class WHERE oid = to_regclass($1)",
		"sage."+last.Name); n != 0 {
		t.Fatalf("%s still exists", last.Name)
	}
	if err := Truncate(ctx, pool, tbl, parts[0]); err != nil {
		t.Fatalf("Truncate history: %v", err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sage."+tbl.Name); n != 0 {
		t.Fatalf("%d rows after truncating history", n)
	}
	if err := Drop(ctx, pool, tbl, Partition{Name: "not_" + tbl.Name}); err == nil {
		t.Fatal("Drop accepted a relation that is not one of the table's partitions")
	}
}

// Dropping a partition needs an exclusive lock on the parent. It must give
// up quickly instead of queueing every reader behind a long transaction.
func TestDrop_GivesUpBehindALongReader(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	converted(t, ctx, pool, tbl)
	if _, err := Ensure(ctx, pool, tbl, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	parts, _ := List(ctx, pool, tbl)
	victim := days(parts)[0]
	reader, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	if _, err := reader.Exec(ctx, "SELECT count(*) FROM sage."+tbl.Name); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = Drop(ctx, pool, tbl, victim)
	if err == nil || time.Since(start) > LockTimeout+5*time.Second {
		t.Fatalf("Drop behind a reader = %v after %s; want a lock timeout", err,
			time.Since(start))
	}
	if !strings.Contains(err.Error(), victim.Name) {
		t.Fatalf("error does not name the partition: %v", err)
	}
}

func TestSizeSumsEveryPartition(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	plain, err := Size(ctx, pool, tbl)
	if err != nil || plain <= 0 {
		t.Fatalf("Size of the plain table = %d, %v", plain, err)
	}
	converted(t, ctx, pool, tbl)
	if _, err := Ensure(ctx, pool, tbl, time.Now(), 3); err != nil {
		t.Fatal(err)
	}
	exec(t, ctx, pool, `INSERT INTO sage.`+tbl.Name+` (at, v) SELECT now(), g
		FROM generate_series(1, 2000) g`)
	got, err := Size(ctx, pool, tbl)
	if err != nil {
		t.Fatal(err)
	}
	want := count(t, ctx, pool, `SELECT sum(pg_total_relation_size(relid))::bigint
		FROM pg_partition_tree($1::regclass)`, "sage."+tbl.Name)
	if got != want || got <= plain {
		t.Fatalf("Size = %d, want %d (> %d)", got, want, plain)
	}
	if _, err := Size(ctx, pool, Table{Name: "no_such_table", Column: "at"}); err == nil {
		t.Fatal("Size of a missing table succeeded")
	}
}

// countingDB counts statements so a Keeper's cache can be checked.
type countingDB struct {
	DB
	mu sync.Mutex
	n  int
}

func (c *countingDB) bump() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *countingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag,
	error) {
	c.bump()
	return c.DB.Exec(ctx, sql, args...)
}

func (c *countingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.bump()
	return c.DB.Query(ctx, sql, args...)
}

func (c *countingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.bump()
	return c.DB.QueryRow(ctx, sql, args...)
}

func (c *countingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	c.bump()
	return c.DB.Begin(ctx)
}

func TestKeeper_EnsuresEachDayOnce(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	converted(t, ctx, pool, tbl)
	db := &countingDB{DB: pool}
	k := NewKeeper()
	at := time.Now()
	if err := k.Ensure(ctx, db, at, tbl); err != nil {
		t.Fatalf("Keeper.Ensure: %v", err)
	}
	first := db.n
	if first == 0 {
		t.Fatal("first Ensure issued no statements")
	}
	for i := 0; i < 5; i++ {
		if err := k.Ensure(ctx, db, at.Add(time.Duration(i)*time.Second), tbl); err != nil {
			t.Fatal(err)
		}
	}
	if db.n != first {
		t.Fatalf("cached Ensure issued %d more statements", db.n-first)
	}
	// The next day is ensured ahead: tomorrow is covered already.
	parts, _ := List(ctx, pool, tbl)
	tomorrowEnd := DayStart(at).Add(48 * time.Hour)
	if d := days(parts); len(d) == 0 || d[len(d)-1].Upper.Before(tomorrowEnd) {
		t.Fatalf("tomorrow not covered: %+v", parts)
	}
	// A plain table is skipped, not an error, and costs one lookup per day.
	plain := scratch(t, ctx, pool, nil)
	if err := k.Ensure(ctx, db, at, plain); err != nil {
		t.Fatalf("Keeper.Ensure(plain) = %v", err)
	}
}

func TestPartitioned(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	if ok, err := Partitioned(ctx, pool, tbl); err != nil || ok {
		t.Fatalf("Partitioned(plain) = %v, %v", ok, err)
	}
	converted(t, ctx, pool, tbl)
	if ok, err := Partitioned(ctx, pool, tbl); err != nil || !ok {
		t.Fatalf("Partitioned(converted) = %v, %v", ok, err)
	}
	if _, err := Partitioned(ctx, pool, Table{Name: "no_such_table", Column: "at"}); err == nil {
		t.Fatal("Partitioned(missing) succeeded")
	}
}
