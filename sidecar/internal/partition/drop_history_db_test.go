package partition

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDropAndDropHistory(t *testing.T) {
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
	// The history partition holds a row 9 days old: it stays while a row at
	// or after keepFrom is there (checked under the drop's own lock).
	hist := parts[0]
	monthAgo := time.Now().AddDate(0, 0, -30)
	if dropped, err := DropHistory(ctx, pool, tbl, hist, monthAgo); err != nil || dropped {
		t.Fatalf("DropHistory with a row to keep = %v, %v; want kept", dropped, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sage."+tbl.HistoryName()); n != 1 {
		t.Fatalf("history holds %d rows after a refused drop, want 1", n)
	}
	if _, err := DropHistory(ctx, pool, tbl, days(parts)[0], time.Now()); err == nil {
		t.Fatal("DropHistory accepted a daily partition")
	}
	if dropped, err := DropHistory(ctx, pool, tbl, hist, time.Now()); err != nil || !dropped {
		t.Fatalf("DropHistory with nothing to keep = %v, %v; want dropped", dropped, err)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM pg_class WHERE oid = to_regclass($1)",
		"sage."+tbl.HistoryName()); n != 0 {
		t.Fatal("the history partition still exists")
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM sage."+tbl.Name); n != 0 {
		t.Fatalf("%d rows after dropping history", n)
	}
	if after, _ := List(ctx, pool, tbl); len(after) != len(parts)-2 {
		t.Fatalf("partitions after = %+v, want %d", after, len(parts)-2)
	}
	if err := Drop(ctx, pool, tbl, Partition{Name: "not_" + tbl.Name}); err == nil {
		t.Fatal("Drop accepted a relation that is not one of the table's partitions")
	}
}

// Dropping the history partition takes the same short lock as any drop:
// behind a long reader it gives up with an error naming the partition, and
// the partition and its rows stay for the next run.
func TestDropHistory_GivesUpBehindALongReader(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := scratch(t, ctx, pool, nil)
	converted(t, ctx, pool, tbl)
	parts, _ := List(ctx, pool, tbl)
	reader, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	if _, err := reader.Exec(ctx, "SELECT count(*) FROM sage."+tbl.Name); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	dropped, err := DropHistory(ctx, pool, tbl, parts[0], time.Now())
	if err == nil || dropped || time.Since(start) > LockTimeout+5*time.Second {
		t.Fatalf("DropHistory behind a reader = %v, %v after %s; want a lock timeout",
			dropped, err, time.Since(start))
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" ||
		!strings.Contains(err.Error(), tbl.HistoryName()) {
		t.Fatalf("error is not a lock timeout naming the partition: %v", err)
	}
	_ = reader.Rollback(ctx)
	if n := count(t, ctx, pool, "SELECT count(*) FROM pg_class WHERE oid = to_regclass($1)",
		"sage."+tbl.HistoryName()); n != 1 {
		t.Fatal("the history partition is gone after a failed drop")
	}
}
