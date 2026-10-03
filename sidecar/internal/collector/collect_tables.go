package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// collectTables pages through pg_class by oid with a keyset cursor that
// lives only for this call (an error mid-pagination must not make the next
// snapshot resume part-way, G1-B18). Each page is bounded by the batch and
// opens no relation; afterwards the ExactSizeTopN largest are measured
// exactly (best effort: on failure the estimates stay).
func (c *Collector) collectTables(ctx context.Context) ([]TableStats, error) {
	batchSize := collectorBatchSize(c.cfg)
	var allTables []TableStats
	var oids []uint32
	var cursor uint32
	for {
		batch, batchOIDs, err := c.collectTableBatch(ctx, cursor, batchSize)
		if err != nil {
			return nil, err
		}
		allTables = append(allTables, batch...)
		oids = append(oids, batchOIDs...)
		if len(batch) < batchSize {
			break
		}
		cursor = batchOIDs[len(batchOIDs)-1]
	}
	c.refineTableSizes(ctx, allTables, oids)
	return allTables, nil
}

func collectorBatchSize(cfg *config.Config) int {
	if cfg.Collector.BatchSize <= 0 {
		return config.DefaultCollectorBatchSize
	}
	return cfg.Collector.BatchSize
}

func (c *Collector) collectTableBatch(
	ctx context.Context, cursor uint32, batchSize int,
) ([]TableStats, []uint32, error) {
	rows, err := c.catalogQuery(ctx, tableStatsSQL, cursor, batchSize)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var batch []TableStats
	var oids []uint32
	for rows.Next() {
		var t TableStats
		var oid uint32
		if err := rows.Scan(
			&t.SchemaName, &t.RelName,
			&t.SeqScan, &t.SeqTupRead, &t.IdxScan, &t.IdxTupFetch,
			&t.NTupIns, &t.NTupUpd, &t.NTupDel, &t.NTupHotUpd,
			&t.NLiveTup, &t.NDeadTup,
			&t.LastVacuum, &t.LastAutovacuum,
			&t.LastAnalyze, &t.LastAutoanalyze,
			&t.VacuumCount, &t.AutovacuumCount,
			&t.AnalyzeCount, &t.AutoanalyzeCount,
			&t.TableBytes, &t.IndexBytes,
			&t.Relpersistence, &t.XIDAge, &oid,
		); err != nil {
			return nil, nil, err
		}
		t.TotalBytes = t.TableBytes + t.IndexBytes
		batch = append(batch, t)
		oids = append(oids, oid)
	}
	return batch, oids, rows.Err()
}

// refineTableSizes replaces the estimates of the largest tables with
// exact sizes. Only these relations are opened, which keeps each pool
// backend's relation cache bounded (static.md F3: one full pass grew it
// to 299 MB on lifeos).
func (c *Collector) refineTableSizes(ctx context.Context, tables []TableStats, oids []uint32) {
	bytes := make([]int64, len(tables))
	for i, t := range tables {
		bytes[i] = t.TotalBytes
	}
	top := topByBytes(oids, bytes, c.exactTopN)
	if len(top) == 0 {
		return
	}
	exact, err := c.exactSizes(ctx, exactTableSizesSQL, top, 2)
	if err != nil {
		c.logFn("WARN", "collector: exact table sizes skipped this cycle "+
			"(relpages estimates kept): %v", err)
		return
	}
	at := make(map[uint32]int, len(oids))
	for i, oid := range oids {
		at[oid] = i
	}
	for oid, v := range exact {
		t := &tables[at[oid]]
		t.TableBytes, t.IndexBytes = v[0], v[1]
		t.TotalBytes = v[0] + v[1]
	}
}

// exactSizes runs a size query over oids; each row is the oid and width
// sizes. Relations dropped since the page was read (NULL) are left out.
func (c *Collector) exactSizes(
	ctx context.Context, sql string, oids []uint32, width int,
) (map[uint32][]int64, error) {
	rows, err := c.catalogQuery(ctx, sql, oids)
	if err != nil {
		return nil, fmt.Errorf("exact sizes: %w", err)
	}
	defer rows.Close()
	out := make(map[uint32][]int64, len(oids))
	for rows.Next() {
		var oid uint32
		vals := make([]*int64, width)
		dest := []any{&oid}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("exact sizes: %w", err)
		}
		if sizes, ok := derefAll(vals); ok {
			out[oid] = sizes
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("exact sizes: %w", err)
	}
	return out, nil
}

func derefAll(vals []*int64) ([]int64, bool) {
	out := make([]int64, len(vals))
	for i, v := range vals {
		if v == nil {
			return nil, false
		}
		out[i] = *v
	}
	return out, true
}

// clock is the collector's time source (tests replace it).
func (c *Collector) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}
