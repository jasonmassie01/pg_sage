package testdb

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent-access test: VacuumAllVisible keeps no state; concurrent
// snapshot holders are the input the holder tests below exercise.

func visibilityPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), CreateDatabase(t, "visibility"))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedVisibilityTable(t *testing.T, pool *pgxpool.Pool, rows int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS public.vis
		(id int PRIMARY KEY, pad text) WITH (autovacuum_enabled = off)`)
	if err != nil {
		t.Fatalf("create public.vis: %v", err)
	}
	_, err = pool.Exec(context.Background(), `INSERT INTO public.vis
		SELECT g, repeat('x', 100) FROM generate_series(
			(SELECT count(*) FROM public.vis) + 1, (SELECT count(*) FROM public.vis) + $1) g`,
		rows)
	if err != nil {
		t.Fatalf("seed public.vis: %v", err)
	}
}

func visiblePages(t *testing.T, pool *pgxpool.Pool) (int, int) {
	t.Helper()
	var visible, pages int
	if err := pool.QueryRow(context.Background(), `SELECT relallvisible, relpages
		FROM pg_class WHERE oid = 'public.vis'::regclass`).Scan(&visible, &pages); err != nil {
		t.Fatalf("read visibility of public.vis: %v", err)
	}
	return visible, pages
}

// holdSnapshot opens a transaction in pool's database whose snapshot
// predates every later write, as an autovacuum ANALYZE's does.
func holdSnapshot(t *testing.T, pool *pgxpool.Pool) (*pgxpool.Conn, int) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}
	t.Cleanup(conn.Release)
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("holder pid: %v", err)
	}
	if _, err := conn.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ"); err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT count(*) FROM pg_class"); err != nil {
		t.Fatalf("holder snapshot: %v", err)
	}
	return conn, pid
}

func TestVacuumAllVisibleCoversEveryPage(t *testing.T) {
	pool := visibilityPool(t)
	seedVisibilityTable(t, pool, 5000)
	if err := VacuumAllVisible(context.Background(), pool, "public.vis",
		30*time.Second); err != nil {
		t.Fatalf("VacuumAllVisible: %v", err)
	}
	visible, pages := visiblePages(t, pool)
	if pages == 0 || visible != pages {
		t.Fatalf("public.vis: %d of %d pages all-visible, want all (> 0)", visible, pages)
	}
}

// One VACUUM cannot mark the pages while an older snapshot of the database
// is open; the helper keeps vacuuming until the holder is gone.
func TestVacuumAllVisibleWaitsOutASnapshotHolder(t *testing.T) {
	pool := visibilityPool(t)
	seedVisibilityTable(t, pool, 10)
	holder, _ := holdSnapshot(t, pool)
	seedVisibilityTable(t, pool, 5000)
	var released atomic.Bool
	releaseDone := make(chan error, 1)
	go func() {
		time.Sleep(700 * time.Millisecond)
		released.Store(true)
		_, err := holder.Exec(context.Background(), "ROLLBACK")
		releaseDone <- err
	}()
	err := VacuumAllVisible(context.Background(), pool, "public.vis", 60*time.Second)
	wasReleased := released.Load()
	if rerr := <-releaseDone; rerr != nil {
		t.Fatalf("release holder: %v", rerr)
	}
	if err != nil {
		t.Fatalf("VacuumAllVisible with a holder that ends: %v", err)
	}
	if !wasReleased {
		t.Fatal("VacuumAllVisible returned while the older snapshot was still open")
	}
	visible, pages := visiblePages(t, pool)
	if pages == 0 || visible != pages {
		t.Fatalf("public.vis: %d of %d pages all-visible, want all", visible, pages)
	}
}

func TestVacuumAllVisibleNamesAHolderThatOutlastsTheTimeout(t *testing.T) {
	pool := visibilityPool(t)
	seedVisibilityTable(t, pool, 10)
	holder, pid := holdSnapshot(t, pool)
	t.Cleanup(func() { _, _ = holder.Exec(context.Background(), "ROLLBACK") })
	seedVisibilityTable(t, pool, 5000)
	err := VacuumAllVisible(context.Background(), pool, "public.vis", time.Second)
	if err == nil {
		t.Fatal("VacuumAllVisible succeeded while an older snapshot stayed open")
	}
	msg := err.Error()
	if !strings.Contains(msg, "all-visible") || !strings.Contains(msg, "public.vis") ||
		!strings.Contains(msg, "pid "+strconv.Itoa(pid)) {
		t.Fatalf("error %q, want the table, the visibility shortfall and holder pid %d",
			msg, pid)
	}
}

func TestVacuumAllVisibleEmptyTableIsCovered(t *testing.T) {
	pool := visibilityPool(t)
	seedVisibilityTable(t, pool, 0)
	if err := VacuumAllVisible(context.Background(), pool, "public.vis",
		5*time.Second); err != nil {
		t.Fatalf("VacuumAllVisible on an empty table: %v", err)
	}
	if visible, pages := visiblePages(t, pool); visible != 0 || pages != 0 {
		t.Fatalf("empty public.vis: %d of %d pages, want 0 of 0", visible, pages)
	}
}

func TestVacuumAllVisibleRejectsAnUnknownTable(t *testing.T) {
	pool := visibilityPool(t)
	err := VacuumAllVisible(context.Background(), pool, "public.no_such_table",
		5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "public.no_such_table") {
		t.Fatalf("error = %v, want one naming public.no_such_table", err)
	}
}

func TestVacuumAllVisibleStopsOnACancelledContext(t *testing.T) {
	pool := visibilityPool(t)
	seedVisibilityTable(t, pool, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := VacuumAllVisible(ctx, pool, "public.vis", 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
