package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// G-P0-12: last_idx_scan (PG16+) is collected so the unused-index rule can
// tell "scanned once, weeks ago" from "in use". Older servers have no such
// column; the collector must keep working there and report nil.
func TestCollectIndexes_LastIdxScan(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.p0_last_scan;
		CREATE TABLE public.p0_last_scan (id int PRIMARY KEY, v int);
		CREATE INDEX p0_last_scan_v ON public.p0_last_scan (v);
		CREATE INDEX p0_never_scanned ON public.p0_last_scan (id, v);
		INSERT INTO public.p0_last_scan SELECT g, g FROM generate_series(1, 2000) g;
		ANALYZE public.p0_last_scan`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.p0_last_scan")
	})
	scanIndex(t, ctx, pool, version)
	// Collect with a deliberately wrong version number: the column must be
	// detected from the server, not from the configured version.
	c := New(pool, testConfig(), 160000, noopLog)
	deadline := time.Now().Add(5 * time.Second)
	for {
		used, never := lastScans(t, ctx, c)
		if version < 160000 {
			if used != nil || never != nil {
				t.Fatalf("PG%d reported last_idx_scan %v / %v, want nil", version, used, never)
			}
			return
		}
		if used != nil {
			if time.Since(*used) > time.Hour || never != nil {
				t.Fatalf("last_idx_scan = %v (never-scanned: %v)", used, never)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("last_idx_scan never appeared for a scanned index")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// scanIndex runs one index scan on p0_last_scan_v and flushes the
// session's statistics (PG15+ flushes on demand; PG14 via the collector).
func scanIndex(t *testing.T, ctx context.Context, pool *pgxpool.Pool, version int) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	const planner = "SET enable_seqscan = off; SET enable_bitmapscan = off"
	if _, err := conn.Exec(ctx, planner); err != nil {
		t.Fatalf("planner settings: %v", err)
	}
	var v int
	if err := conn.QueryRow(ctx,
		"SELECT v FROM public.p0_last_scan WHERE v = 5").Scan(&v); err != nil || v != 5 {
		t.Fatalf("index scan = %d, %v", v, err)
	}
	if version >= 150000 {
		if _, err := conn.Exec(ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
			t.Fatalf("force flush: %v", err)
		}
	}
	if _, err := conn.Exec(ctx, "RESET ALL; SELECT 1"); err != nil {
		t.Fatalf("flush trigger: %v", err)
	}
}

func lastScans(t *testing.T, ctx context.Context, c *Collector) (used, never *time.Time) {
	t.Helper()
	got, err := c.collectIndexes(ctx)
	if err != nil {
		t.Fatalf("collectIndexes: %v", err)
	}
	for _, idx := range got {
		switch {
		case strings.EqualFold(idx.IndexRelName, "p0_last_scan_v"):
			used = idx.LastIdxScan
		case strings.EqualFold(idx.IndexRelName, "p0_never_scanned"):
			never = idx.LastIdxScan
		}
	}
	return used, never
}
