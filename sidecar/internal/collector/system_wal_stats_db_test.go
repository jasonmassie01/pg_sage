package collector

import (
	"context"
	"testing"
	"time"
)

// advisors-05 on real PostgreSQL (14+): pg_stat_wal and the checkpointer
// view are read every cycle. An explicit CHECKPOINT after a write burst
// moves the requested count, and pg_stat_wal.wal_bytes grows with the
// burst under an unchanged epoch.
func TestSystemWALStatsDB_CheckpointAndWriteBurst(t *testing.T) {
	pool, c := sysIntervalCollectorOn(t,
		exclusivePool(t, "cluster WAL bytes and requested checkpoints"))
	ctx := context.Background()
	first := collectAndPublish(t, c)
	if _, err := pool.Exec(ctx, `INSERT INTO public.iv_t
		SELECT g, repeat('w', 1000) FROM generate_series(1, 3000) g`); err != nil {
		t.Fatalf("write burst: %v", err)
	}
	if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// Backends report their WAL usage asynchronously (PG14's collector
	// every 500 ms; PG15+ at most once a second, or 10 s once idle), so the
	// burst shows up in pg_stat_wal a little later: collect until it does.
	second := collectAndPublish(t, c)
	for deadline := time.Now().Add(20 * time.Second); second.System.WALBytes-
		first.System.WALBytes < 3000*1000 && time.Now().Before(deadline); {
		time.Sleep(500 * time.Millisecond)
		second = collectAndPublish(t, c)
	}
	f, s := first.System, second.System
	if f.WALStatsEpoch.IsZero() || !f.WALStatsEpoch.Equal(s.WALStatsEpoch) {
		t.Fatalf("WAL stats epoch %v -> %v, want known and unchanged", f.WALStatsEpoch,
			s.WALStatsEpoch)
	}
	if s.WALBytes-f.WALBytes < 3000*1000 {
		t.Errorf("pg_stat_wal.wal_bytes moved %d -> %d over a 3 MB burst", f.WALBytes,
			s.WALBytes)
	}
	if s.CheckpointsRequested <= f.CheckpointsRequested {
		t.Errorf("requested checkpoints %d -> %d, want CHECKPOINT counted",
			f.CheckpointsRequested, s.CheckpointsRequested)
	}
	if s.CheckpointsTimed < f.CheckpointsTimed ||
		s.TotalCheckpoints != s.CheckpointsTimed+s.CheckpointsRequested {
		t.Errorf("timed %d -> %d, total %d: want the split summing to the total",
			f.CheckpointsTimed, s.CheckpointsTimed, s.TotalCheckpoints)
	}
	if f.CheckpointStatsEpoch.IsZero() ||
		!f.CheckpointStatsEpoch.Equal(s.CheckpointStatsEpoch) {
		t.Errorf("checkpoint epoch %v -> %v, want known and unchanged",
			f.CheckpointStatsEpoch, s.CheckpointStatsEpoch)
	}
}
