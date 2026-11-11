package collector

import (
	"context"
	"testing"
)

// advisors-05 on real PostgreSQL (14+): pg_stat_wal and the checkpointer
// view are read every cycle. An explicit CHECKPOINT after a write burst
// moves the requested count, and pg_stat_wal.wal_bytes grows with the
// burst under an unchanged epoch.
func TestSystemWALStatsDB_CheckpointAndWriteBurst(t *testing.T) {
	pool, c := sysIntervalCollector(t)
	ctx := context.Background()
	first := collectAndPublish(t, c)
	if _, err := pool.Exec(ctx, `INSERT INTO public.iv_t
		SELECT g, repeat('w', 1000) FROM generate_series(1, 3000) g`); err != nil {
		t.Fatalf("write burst: %v", err)
	}
	if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Logf("pg_stat_force_next_flush unavailable (PG14 flushes itself): %v", err)
	}
	second := collectAndPublish(t, c)
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
