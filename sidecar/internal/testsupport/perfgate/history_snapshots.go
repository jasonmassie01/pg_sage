package perfgate

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testsupport/snapfixture"
)

// snapshotCycles is the history written through the real snapshot writer
// (keyframes, checkpoints and deltas exactly as the collector stores
// them); it is then replicated back in time to reach the row target.
const snapshotCycles = 240

// seedSnapshots writes snapshotCycles one-minute cycles of a 50-table,
// 150-index catalog through snapstore.Writer, then copies those rows
// further into the past until sage.snapshots holds n rows. A copied delta
// keeps its base_id, so every row still reads back.
func seedSnapshots(ctx context.Context, pool *pgxpool.Pool, n int) error {
	sc := snapfixture.Scenario{
		Start:  time.Now().UTC().Add(-snapshotCycles * time.Minute).Truncate(time.Minute),
		Step:   time.Minute,
		Cycles: snapshotCycles, Tables: 50, Indexes: 150, Seed: 78,
		Events: snapfixture.Events{QueryChurn: 5},
	}
	cycles, err := sc.Generate()
	if err != nil {
		return fmt.Errorf("perfgate: generate snapshot history: %w", err)
	}
	w := snapstore.NewWriter()
	for _, c := range cycles {
		rows := make([]snapstore.Row, 0, len(c.Docs))
		for _, d := range c.Docs {
			rows = append(rows, snapstore.Row{Category: d.Category, Data: d.Data})
		}
		if err := w.Persist(ctx, pool, c.At, rows); err != nil {
			return fmt.Errorf("perfgate: persist snapshot cycle: %w", err)
		}
	}
	return replicateSnapshots(ctx, pool, n)
}

// replicateSnapshots copies the written rows back in time, one span of
// snapshotCycles+1 minutes per copy, until there are n rows in all.
func replicateSnapshots(ctx context.Context, pool *pgxpool.Pool, n int) error {
	_, err := pool.Exec(ctx, `WITH src AS (
		    SELECT id, collected_at, category, data, base_id FROM sage.snapshots),
		  total AS (SELECT count(*) AS c FROM src)
		INSERT INTO sage.snapshots (collected_at, category, data, base_id)
		SELECT s.collected_at - k * make_interval(mins => $2::int + 1), s.category,
		       s.data, s.base_id
		FROM src s, total, generate_series(1, ($1::int / greatest(total.c, 1))::int + 1) k
		ORDER BY k, s.id
		LIMIT greatest($1::int - (SELECT c FROM total), 0)`, n, snapshotCycles)
	if err != nil {
		return fmt.Errorf("perfgate: replicate snapshot history: %w", err)
	}
	return nil
}
