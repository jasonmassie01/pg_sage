package srebench

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// disk-slow-consumer-fill failed twice in CI ("slot ... does not retain
// the written WAL", runs 37160511933 and 37217656844). The logical slot
// retains exactly what was written after its creation: 18 MiB of
// messages plus about 70-150 KB of record and page headers, against an
// 18 MiB check. The wraparound scenarios that run first in the same shard
// burned XIDs with a session-level synchronous_commit = off on a pooled
// connection; a later emitWAL on that connection committed
// asynchronously, so pg_current_wal_lsn() (the write position) lagged the
// inserted WAL by up to wal_buffers (2.6 MB measured on PG17 right after
// 6 MiB) and the slot looked short of the WAL just written.

// onePool is a pool with a single connection, so every statement shares
// one session; init runs on it once.
func onePool(t *testing.T, init string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	if init != "" {
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, init)
			return err
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func showSetting(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(ctx, "SELECT current_setting($1)", name).Scan(&v); err != nil {
		t.Fatalf("show %s: %v", name, err)
	}
	return v
}

// Burning XIDs fast must not leave the pooled session committing
// asynchronously for every later fault program.
func TestBurnXIDs_LeavesSessionSettingsAlone(t *testing.T) {
	pool, ctx := onePool(t, "")
	before := showSetting(t, ctx, pool, "synchronous_commit")
	e := &Env{Pool: pool}
	x0, err := e.nextXID(ctx)
	if err != nil {
		t.Fatalf("next xid: %v", err)
	}
	if err := e.burnXIDs(ctx, 50); err != nil {
		t.Fatalf("burn: %v", err)
	}
	x1, err := e.nextXID(ctx)
	if err != nil {
		t.Fatalf("next xid: %v", err)
	}
	if x1-x0 < 50 {
		t.Fatalf("burned %d XIDs, want at least 50", x1-x0)
	}
	if after := showSetting(t, ctx, pool, "synchronous_commit"); after != before {
		t.Fatalf("synchronous_commit = %q after burning XIDs, want the session's %q",
			after, before)
	}
}

// The WAL a fault program writes is written (not only inserted) when
// emitWAL returns, even on a session that commits asynchronously (a leak
// from another program or a server default).
func TestEmitWAL_WrittenWhenItReturns(t *testing.T) {
	pool, ctx := onePool(t, "SET synchronous_commit = off")
	if got := showSetting(t, ctx, pool, "synchronous_commit"); got != "off" {
		t.Fatalf("setup: synchronous_commit = %q, want off", got)
	}
	e := &Env{Pool: pool}
	var start string
	if err := pool.QueryRow(ctx, "SELECT pg_current_wal_insert_lsn()::text").
		Scan(&start); err != nil {
		t.Fatalf("insert lsn: %v", err)
	}
	const mb = 4
	if err := e.emitWAL(ctx, mb); err != nil {
		t.Fatalf("emit: %v", err)
	}
	var written int64
	if err := pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), $1::pg_lsn)::int8`,
		start).Scan(&written); err != nil {
		t.Fatalf("written: %v", err)
	}
	if written < mb<<20 {
		t.Fatalf("written WAL = %d bytes when emitWAL returned, want at least %d", written,
			mb<<20)
	}
	if got := showSetting(t, ctx, pool, "synchronous_commit"); got != "off" {
		t.Fatalf("emitWAL changed the session's synchronous_commit to %q", got)
	}
}
