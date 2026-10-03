package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// statsInterval14 is PostgreSQL 14's PGSTAT_STAT_INTERVAL: a session
// reports its statistics at most this often, and only as it goes idle.
const statsInterval14 = 500 * time.Millisecond

// FlushStats makes the cumulative statistics conn's session has counted
// so far visible to every new reader. A session that reported less than
// the flush interval ago keeps its next counts until it is next idle
// after the interval (PostgreSQL 15+: up to 10 s later; 14: not until it
// runs another statement), so a fixed sleep is no flush.
//
// PostgreSQL 15+ flushes on pg_stat_force_next_flush() as the statement
// ends. PostgreSQL 14 has no such function and its collector applies a
// report asynchronously: FlushStats waits out the interval, scans a fresh
// sentinel table (its counts are the report's last entry) and waits until
// the collector shows that scan.
func FlushStats(ctx context.Context, conn Execer) error {
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		return fmt.Errorf("flush statistics: read server version: %w", err)
	}
	if version >= 150000 {
		if _, err := conn.Exec(ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
			return fmt.Errorf("flush statistics: %w", err)
		}
		return nil
	}
	return flushStats14(ctx, conn)
}

func flushStats14(ctx context.Context, conn Execer) error {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("flush statistics: sentinel name: %w", err)
	}
	sentinel := pgx.Identifier{"public", "testdb_flush_" + hex.EncodeToString(suffix)}.
		Sanitize()
	if _, err := conn.Exec(ctx, "CREATE TABLE "+sentinel+" (x int)"); err != nil {
		return fmt.Errorf("flush statistics: create sentinel: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS "+sentinel) }()
	if err := sleepCtx(ctx, statsInterval14+100*time.Millisecond); err != nil {
		return fmt.Errorf("flush statistics: %w", err)
	}
	// Going idle after this scan reports everything pending, the sentinel last.
	if _, err := conn.Exec(ctx, "SELECT count(*) FROM "+sentinel); err != nil {
		return fmt.Errorf("flush statistics: scan sentinel: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var scans int64
		if _, err := conn.Exec(ctx, "SELECT pg_stat_clear_snapshot()"); err != nil {
			return fmt.Errorf("flush statistics: %w", err)
		}
		if err := conn.QueryRow(ctx, `SELECT COALESCE(pg_stat_get_numscans($1::regclass), 0)`,
			sentinel).Scan(&scans); err != nil {
			return fmt.Errorf("flush statistics: read sentinel: %w", err)
		}
		if scans > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("flush statistics: the collector did not apply the report in 30s")
		}
		if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
			return fmt.Errorf("flush statistics: %w", err)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
