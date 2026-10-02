package runway

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Integration: against real PostgreSQL the monitor samples a sequence
// once per sequence interval while the minute-cadence series (WAL
// position) are sampled every tick.
func TestMonitorTick_SamplesSequencesOnTheirCadence(t *testing.T) {
	pool, ctx := livePool(t)
	seq := fmt.Sprintf("mon_cadence_%d_seq", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SEQUENCE %[1]s AS integer;
		SELECT setval('%[1]s', 2147000000)`, seq)); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SEQUENCE IF EXISTS "+seq)
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.runway_samples WHERE subject = $1", "public."+seq)
	})
	opts := testOptions()
	opts.Retention, opts.Interval = 48*time.Hour, time.Minute
	opts.SequenceInterval = 10 * time.Minute
	m, err := NewMonitor(pool, probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1)), nil, opts, func(string, string, ...any) {})
	if err != nil {
		t.Fatalf("NewMonitor: %v", err)
	}
	start := time.Now().Add(-time.Second)
	for i := 0; i < 3; i++ {
		if _, err := m.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	var seqSamples, walSamples int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE kind = 'sequence' AND subject = $1),
		count(*) FILTER (WHERE kind = 'wal_position' AND subject = 'cluster')
		FROM sage.runway_samples WHERE sampled_at >= $2`, "public."+seq, start).
		Scan(&seqSamples, &walSamples); err != nil {
		t.Fatalf("count: %v", err)
	}
	if seqSamples != 1 || walSamples < 3 {
		t.Fatalf("samples: sequence %d, wal %d; want 1 sequence sample and one WAL "+
			"sample per tick", seqSamples, walSamples)
	}
}
