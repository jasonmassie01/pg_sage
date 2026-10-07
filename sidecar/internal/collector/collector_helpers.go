package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/partition"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// detectStatsReset returns true if pg_stat_statements was likely
// reset. Both conditions must be met: >50% of overlapping queries
// show decreased call counts AND total calls dropped by >80%.
// This avoids false positives from natural workload churn where
// individual queries rotate but aggregate call volume stays stable.
func detectStatsReset(current, previous []QueryStats) bool {
	prevTotal := sumCalls(previous)
	if prevTotal == 0 {
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
	ratioDecreased := float64(decreased) / float64(compared)
	currTotal := sumCalls(current)
	return ratioDecreased > 0.5 && currTotal < prevTotal/5
}

// sumCalls returns the total number of calls across all queries.
func sumCalls(qs []QueryStats) int64 {
	var total int64
	for _, q := range qs {
		total += q.Calls
	}
	return total
}

// collectStatStatementsMax queries pg_stat_statements.max setting.
func (c *Collector) collectStatStatementsMax(
	ctx context.Context,
) int {
	var val int
	err := c.catalogQueryRow(
		ctx,
		`/* pg_sage */ SELECT setting::int FROM pg_settings
		 WHERE name = 'pg_stat_statements.max'`,
	).Scan(&val)
	if err != nil {
		// Extension may not be loaded; non-fatal.
		return 0
	}
	return val
}

// persist writes the snapshot into the history store's sage.snapshots, one
// row per available category; catalog categories are delta encoded by the
// snapshot store.
func (c *Collector) persist(ctx context.Context, snap *Snapshot) error {
	rows, err := snapshotRows(snap)
	if err != nil {
		return err
	}
	c.ensurePartitions(ctx, snap.CollectedAt)
	return c.snapWriter.Persist(ctx, c.pool, snap.CollectedAt, rows)
}

// ensurePartitions makes sure the day partitions of at (and the next day)
// exist before rows are written there, in the history store (the meta
// database in history.store: meta). A failure is logged, not fatal: a row
// with no day partition lands in the default partition.
func (c *Collector) ensurePartitions(ctx context.Context, at time.Time) {
	err := c.partitions.Ensure(ctx, histstore.Resolve(c.pool), at, partition.Snapshots,
		partition.QueryStore)
	if err != nil {
		c.logFn("WARN", "ensure sage history partitions: %v", err)
	}
}

// snapshotRows marshals each available category of snap, sorted by
// category. An unavailable category is unknown, not empty: no row.
func snapshotRows(snap *Snapshot) ([]snapstore.Row, error) {
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
	rows := make([]snapstore.Row, 0, len(categories))
	for cat, data := range categories {
		if !snap.Available(cat) {
			continue
		}
		j, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("marshal %s snapshot: %w", cat, err)
		}
		rows = append(rows, snapstore.Row{Category: cat, Data: j})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Category < rows[b].Category })
	return rows, nil
}
