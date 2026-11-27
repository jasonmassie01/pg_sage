package facts

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// PostgreSQL 14's statistics collector takes each session's reports as UDP
// datagrams. It applies one session's datagrams in the order they were sent
// and drops any that arrive while its receive buffer is full. A session
// reports at most every 500 ms (PGSTAT_STAT_INTERVAL), and only when it
// goes idle.
const (
	// reportInterval14 is that interval plus a margin. After this long idle,
	// the session's next statement reports everything the session holds.
	reportInterval14 = 600 * time.Millisecond
	// sentinelRescan is how long a sentinel scan may take to show before
	// the flush scans the sentinel again.
	sentinelRescan = time.Second
	// sentinelTimeout bounds the wait for a sentinel scan to show.
	sentinelTimeout = 30 * time.Second
)

// No concurrent-access test: the flush works only on the sessions it holds
// and keeps no state.

// flushSetupStats makes sure every count that the pool's idle sessions hold
// has been applied or lost for good, so no setup count reaches a later read.
// On PostgreSQL 15+ it calls testdb.FlushStats (pg_stat_force_next_flush).
// On 14, testdb.FlushStats waits for a single sentinel scan. That scan is
// reported last, after every table the setup left pending. In the
// 200-schema test that is a long burst of datagrams, and the last one is the
// likeliest to find the receive buffer full. A dropped scan never shows, and
// that wait fails after 30 s. flushSession14 scans the sentinel again instead.
func flushSetupStats(ctx context.Context, pool *pgxpool.Pool) error {
	conns := pool.AcquireAllIdle(ctx)
	defer func() {
		for _, c := range conns {
			c.Release()
		}
	}()
	for _, c := range conns {
		var version int
		if err := c.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
			Scan(&version); err != nil {
			return fmt.Errorf("flush statistics: read server version: %w", err)
		}
		flush := flushSession14
		if version >= 150000 {
			flush = testdb.FlushStats
		}
		if err := flush(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// flushSession14 flushes one PostgreSQL 14 session. It creates a fresh
// sentinel table, waits out the report interval and scans the sentinel, so
// the report sent when the session goes idle carries everything the session
// holds, with the sentinel scan last. It then waits until a scan shows.
func flushSession14(ctx context.Context, conn testdb.Execer) error {
	sentinel := pgx.Identifier{"public", "facts_flush_" + strings.ToLower(rand.Text())}.
		Sanitize()
	if _, err := conn.Exec(ctx, "CREATE TABLE "+sentinel+" (x int)"); err != nil {
		return fmt.Errorf("flush statistics: create sentinel: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+sentinel) }()
	if err := scanAfterInterval(ctx, conn, sentinel); err != nil {
		return err
	}
	return awaitScan(ctx, conn, sentinel, sentinelTimeout)
}

// scanAfterInterval waits out the report interval, then scans sentinel, so
// the session sends a report containing the scan when it goes idle.
func scanAfterInterval(ctx context.Context, conn testdb.Execer, sentinel string) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("flush statistics: %w", ctx.Err())
	case <-time.After(reportInterval14):
	}
	if _, err := conn.Exec(ctx, "SELECT count(*) FROM "+sentinel); err != nil {
		return fmt.Errorf("flush statistics: scan sentinel: %w", err)
	}
	return nil
}

// awaitScan waits until a fresh read of the statistics shows a scan of
// sentinel. If no scan has shown within sentinelRescan, the datagram may
// have been dropped, and then it never shows, so awaitScan scans the
// sentinel again. Each scan goes out in a fresh report. The collector
// applies a session's datagrams in order, so the first scan to show proves
// that every report the session sent before it was applied or lost.
// awaitScan gives up after timeout, so a collector that applies nothing
// still fails.
func awaitScan(ctx context.Context, conn testdb.Execer, sentinel string,
	timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		for rescan := time.Now().Add(sentinelRescan); time.Now().Before(rescan); {
			scans, err := sentinelScans(ctx, conn, sentinel)
			if err != nil || scans > 0 {
				return err
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("flush statistics: %w", ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("flush statistics: no scan of %s showed in %s", sentinel, timeout)
		}
		if err := scanAfterInterval(ctx, conn, sentinel); err != nil {
			return err
		}
	}
}

// sentinelScans is the number of sentinel scans that a fresh read of the
// statistics shows.
func sentinelScans(ctx context.Context, conn testdb.Execer, sentinel string) (int64, error) {
	if _, err := conn.Exec(ctx, "SELECT pg_stat_clear_snapshot()"); err != nil {
		return 0, fmt.Errorf("flush statistics: %w", err)
	}
	var scans int64
	if err := conn.QueryRow(ctx, "SELECT COALESCE(pg_stat_get_numscans($1::regclass), 0)",
		sentinel).Scan(&scans); err != nil {
		return 0, fmt.Errorf("flush statistics: read sentinel: %w", err)
	}
	return scans, nil
}

// holdSession holds one pool session for the test.
func holdSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *pgxpool.Conn {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(conn.Release)
	return conn
}

// A scan that never reached the statistics, such as one in a datagram the
// PostgreSQL 14 collector dropped, leaves nothing visible and nothing
// pending. A sentinel that was never scanned is in the same state.
// awaitScan must scan it again and return only once that scan is visible
// to any new reader. Waiting on the first scan alone fails after 30 s.
func TestAwaitScanScansAgainWhenNoScanShows(t *testing.T) {
	pool, ctx := livePool(t)
	sentinel := pgx.Identifier{"public", uniqueName("facts_lost_")}.Sanitize()
	execAll(t, ctx, pool, "CREATE TABLE "+sentinel+" (x int)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+sentinel)
	})
	conn := holdSession(t, ctx, pool)
	if before, err := sentinelScans(ctx, conn, sentinel); err != nil || before != 0 {
		t.Fatalf("scans before awaitScan = %d (%v), want 0", before, err)
	}
	if err := awaitScan(ctx, conn, sentinel, sentinelTimeout); err != nil {
		t.Fatalf("awaitScan: %v", err)
	}
	var scans int64
	if err := pool.QueryRow(ctx, "SELECT COALESCE(pg_stat_get_numscans($1::regclass), 0)",
		sentinel).Scan(&scans); err != nil || scans < 1 {
		t.Fatalf("a new reader sees %d scans (%v) after awaitScan, want at least 1",
			scans, err)
	}
}

// A view counts no scans, like a collector that applies nothing.
// awaitScan must fail at its timeout and name the sentinel, not wait on.
func TestAwaitScanFailsWhenNoScanEverShows(t *testing.T) {
	pool, ctx := livePool(t)
	view := pgx.Identifier{"public", uniqueName("facts_noscan_")}.Sanitize()
	execAll(t, ctx, pool, "CREATE VIEW "+view+" AS SELECT 1 AS x")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP VIEW IF EXISTS "+view) })
	conn := holdSession(t, ctx, pool)
	started := time.Now()
	err := awaitScan(ctx, conn, view, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "no scan of "+view+" showed in 2s") {
		t.Fatalf("awaitScan on a view: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 2*time.Second {
		t.Fatalf("gave up after %s, before the 2s timeout", elapsed)
	}
}

// A failed flush stops at once with the context's error and gives every
// session it took back to the pool, so the pool's Close cannot hang.
func TestFlushSetupStatsStopsOnACancelledContextAndReleasesSessions(t *testing.T) {
	pool, ctx := livePool(t)
	a, b := holdSession(t, ctx, pool), holdSession(t, ctx, pool)
	for _, c := range []*pgxpool.Conn{a, b} {
		if _, err := c.Exec(ctx, "SELECT 1"); err != nil {
			t.Fatalf("select: %v", err)
		}
	}
	a.Release()
	b.Release()
	idle := pool.Stat().IdleConns()
	if idle < 2 {
		t.Fatalf("%d idle sessions, want at least 2", idle)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := flushSetupStats(cancelled, pool); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if st := pool.Stat(); st.AcquiredConns() != 0 || st.IdleConns() != idle {
		t.Fatalf("after the failed flush: %d acquired, %d idle; want 0 and %d",
			st.AcquiredConns(), st.IdleConns(), idle)
	}
}
