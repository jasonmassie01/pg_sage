package collector

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// detectStatsReset returns true if >50% of queries that exist in
// both current and previous snapshots show decreased call counts,
// indicating pg_stat_statements was reset.
func detectStatsReset(current, previous []QueryStats) bool {
	if len(previous) == 0 {
		return false
	}
	prevCalls := make(map[int64]int64, len(previous))
	for _, q := range previous {
		prevCalls[q.QueryID] = q.Calls
	}
	decreased := 0
	compared := 0
	for _, q := range current {
		if prev, ok := prevCalls[q.QueryID]; ok {
			compared++
			if q.Calls < prev {
				decreased++
			}
		}
	}
	if compared == 0 {
		return false
	}
	return float64(decreased)/float64(compared) > 0.5
}

// collectStatStatementsMax queries pg_stat_statements.max setting.
func (c *Collector) collectStatStatementsMax(
	ctx context.Context,
) int {
	var val int
	err := c.pool.QueryRow(
		ctx,
		`SELECT setting::int FROM pg_settings
		 WHERE name = 'pg_stat_statements.max'`,
	).Scan(&val)
	if err != nil {
		// Extension may not be loaded; non-fatal.
		return 0
	}
	return val
}

// persist inserts the snapshot into sage.snapshots
// (one row per category).
func (c *Collector) persist(
	ctx context.Context,
	snap *Snapshot,
) error {
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	categories := map[string]any{
		"queries":      snap.Queries,
		"tables":       snap.Tables,
		"indexes":      snap.Indexes,
		"foreign_keys": snap.ForeignKeys,
		"system":       snap.System,
		"locks":        snap.Locks,
		"sequences":    snap.Sequences,
		"replication":  snap.Replication,
		"io":           snap.IO,
		"partitions":   snap.Partitions,
		"config_data":  snap.ConfigData,
	}

	const insertSQL = `
INSERT INTO sage.snapshots (collected_at, category, data)
VALUES ($1, $2, $3)`

	for cat, data := range categories {
		j, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			ctx, insertSQL, snap.CollectedAt, cat, j,
		); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}
