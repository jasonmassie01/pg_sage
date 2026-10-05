package srebench

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/runway"
)

// Runway fault programs build their trends with the real runway sampler
// on a compressed timescale: a few samples a fraction of a second apart,
// with the fault progressing between them. The bench database's samples
// are cleared first, so a trend never mixes scenarios.

const (
	runwaySamples = 4
	runwayGap     = 400 * time.Millisecond
)

// sampleRunway clears the runway samples, then samples runwaySamples
// times, running step (when set) before every sample after the first.
func (e *Env) sampleRunway(ctx context.Context,
	step func(ctx context.Context, i int) error) error {
	if err := e.clearRunway(ctx); err != nil {
		return err
	}
	m, err := runway.NewMonitor(e.Pool, e.Runner, nil, runway.Options{Database: "bench",
		Interval: time.Minute, Lookback: 6 * time.Hour, MinSamples: 3, MinSpan: time.Second,
		Retention:             48 * time.Hour,
		WALRetainedLimitBytes: float64(autonomy.DefaultWALBackstopBytes)}, nil)
	if err != nil {
		return err
	}
	for i := 0; i < runwaySamples; i++ {
		if i > 0 {
			if step != nil {
				if err := step(ctx, i); err != nil {
					return fmt.Errorf("runway step %d: %w", i, err)
				}
			}
			if err := sleepRest(ctx, runwayGap); err != nil {
				return err
			}
		}
		if _, err := m.Sample(ctx); err != nil {
			return fmt.Errorf("runway sample %d: %w", i+1, err)
		}
	}
	return nil
}

func (e *Env) clearRunway(ctx context.Context) error {
	_, err := e.Pool.Exec(ctx, "DELETE FROM sage.runway_samples")
	return err
}

// burnXIDs consumes n transaction IDs, one committed transaction each.
// Each commits asynchronously (fast) with a transaction-local setting: a
// session-level one stayed on the pooled connection, and a later fault
// program's WAL was then not yet written when its manifestation was read.
func (e *Env) burnXIDs(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	return e.simple(ctx, fmt.Sprintf(`DO $$ BEGIN
		FOR i IN 1..%d LOOP
			PERFORM set_config('synchronous_commit', 'off', true);
			PERFORM pg_current_xact_id();
			COMMIT;
		END LOOP; END $$`, n))
}

// nextXID is the cluster's next transaction ID.
func (e *Env) nextXID(ctx context.Context) (int64, error) {
	var x int64
	err := e.Pool.QueryRow(ctx,
		"SELECT pg_snapshot_xmax(pg_current_snapshot())::text::int8").Scan(&x)
	return x, err
}

// xidRate measures the cluster's XID consumption from start to now.
func (e *Env) xidRate(ctx context.Context, from int64, at time.Time) (float64, error) {
	x, err := e.nextXID(ctx)
	if err != nil {
		return 0, err
	}
	return float64(x-from) / time.Since(at).Seconds(), nil
}

// emitWAL writes about mb MiB of WAL without growing any table: one
// transactional 1 MiB message per transaction, each committed
// synchronously, so the WAL is written (pg_current_wal_lsn) when emitWAL
// returns whatever the session's synchronous_commit.
func (e *Env) emitWAL(ctx context.Context, mb int) error {
	for i := 0; i < mb; i++ {
		if err := e.simple(ctx, `BEGIN;
			SET LOCAL synchronous_commit = on;
			SELECT pg_logical_emit_message(true, 'bench', repeat('w', 1048576));
			COMMIT`); err != nil {
			return err
		}
	}
	return nil
}

// otherDatabasesBytes is the size of every database but this one: other
// test packages creating or dropping fixtures change it.
func (e *Env) otherDatabasesBytes(ctx context.Context) (float64, error) {
	var b float64
	err := e.Pool.QueryRow(ctx, `SELECT COALESCE(sum(pg_database_size(oid)), 0)::float8
		FROM pg_database WHERE datallowconn AND datname <> current_database()`).Scan(&b)
	return b, err
}
