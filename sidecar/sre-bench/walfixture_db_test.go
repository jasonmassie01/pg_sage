package srebench

import (
	"context"
	"slices"
	"testing"
)

// The WAL fault programs write their surge into bench_wal; nothing else
// may write WAL behind them. Autovacuum, vacuuming and analyzing the fresh
// 48 MiB table after a checkpoint, set its hint bits and visibility, and
// with data checksums (on by default from PostgreSQL 18) every first change
// of a page after a checkpoint is a full-page image: reading the table once
// wrote 46 MiB of WAL on PG18 (0 on PG17). A logical consumer confirms the
// walsender's position only at its next status update, so a burst just
// before a sample shows as tens of MiB sent but not flushed (24 and 32 MiB
// measured, against at most 3.4 MiB without it), and the keeping-up decoy
// can be diagnosed standby_flush_backlog, as it once was on CI (PG18,
// causal-graph+llm arm).
func TestWALTableIsLeftToTheScenarios(t *testing.T) {
	ctx, e := liveEnv(t)
	t.Cleanup(func() { _ = truncateWAL(context.Background(), e) })
	if err := writeWAL(1)(ctx, e); err != nil {
		t.Fatalf("write WAL: %v", err)
	}
	var options []string
	if err := e.Pool.QueryRow(ctx, `SELECT COALESCE(reloptions, '{}') FROM pg_catalog.pg_class
		WHERE oid = 'bench_wal'::regclass`).Scan(&options); err != nil {
		t.Fatalf("read bench_wal options: %v", err)
	}
	if !slices.Contains(options, "autovacuum_enabled=false") {
		t.Fatalf("bench_wal options %v, want autovacuum_enabled=false: autovacuum would "+
			"write WAL behind the scenario's surge", options)
	}
}
