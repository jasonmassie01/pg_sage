package perfgate

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The fixture's load and VACUUM dirty gigabytes; without a checkpoint
// before the measured phases, the server's next checkpoint flushed them
// while pg_sage's catalog reads were timed (two of three verification runs
// on 2026-10-09 failed on catalog reads 1.4-2x their usual time on runners
// only 10-14% slower). Settle checkpoints, so the gate measures pg_sage on
// a settled database as a long-running deployment would be.
func TestSettleCheckpoints(t *testing.T) {
	pool, ctx := livePool(t)
	before := checkpointsRequested(t, ctx, pool)
	if err := Settle(ctx, pool); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if after := checkpointsRequested(t, ctx, pool); after <= before {
		t.Fatalf("requested checkpoints %d -> %d: Settle did not checkpoint", before, after)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := Settle(canceled, pool); err == nil {
		t.Fatal("settle on a canceled context returned no error")
	}
	if err := Settle(ctx, nil); err == nil {
		t.Fatal("settle without a pool returned no error")
	}
}

func checkpointsRequested(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	q := "SELECT checkpoints_req FROM pg_stat_bgwriter"
	if version >= 170000 {
		q = "SELECT num_requested FROM pg_stat_checkpointer"
	}
	var n int64
	if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatalf("read checkpoints: %v", err)
	}
	return n
}
