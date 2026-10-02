package collector

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/config"
)

// collectTables pages through pg_stat_user_tables by relid with a keyset
// cursor that lives only for this call (an error mid-pagination must not
// make the next snapshot resume part-way, G1-B18). Paging by oid keeps
// each page bounded by the batch: the old (schema, name) keyset sorted
// the whole catalog for every page (dogfood lifeos-1: 15,301 tables).
func (c *Collector) collectTables(ctx context.Context) ([]TableStats, error) {
	batchSize := collectorBatchSize(c.cfg)
	var allTables []TableStats
	var cursor uint32
	for {
		batch, last, err := c.collectTableBatch(ctx, cursor, batchSize)
		if err != nil {
			return nil, err
		}
		allTables = append(allTables, batch...)
		if len(batch) < batchSize {
			return allTables, nil
		}
		cursor = last
	}
}

func collectorBatchSize(cfg *config.Config) int {
	if cfg.Collector.BatchSize <= 0 {
		return config.DefaultCollectorBatchSize
	}
	return cfg.Collector.BatchSize
}

func (c *Collector) collectTableBatch(
	ctx context.Context, cursor uint32, batchSize int,
) ([]TableStats, uint32, error) {
	rows, err := c.catalogQuery(ctx, tableStatsSQL, cursor, batchSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var batch []TableStats
	var last uint32
	for rows.Next() {
		var t TableStats
		if err := rows.Scan(
			&t.SchemaName, &t.RelName,
			&t.SeqScan, &t.SeqTupRead, &t.IdxScan, &t.IdxTupFetch,
			&t.NTupIns, &t.NTupUpd, &t.NTupDel, &t.NTupHotUpd,
			&t.NLiveTup, &t.NDeadTup,
			&t.LastVacuum, &t.LastAutovacuum,
			&t.LastAnalyze, &t.LastAutoanalyze,
			&t.VacuumCount, &t.AutovacuumCount,
			&t.AnalyzeCount, &t.AutoanalyzeCount,
			&t.TotalBytes, &t.TableBytes, &t.IndexBytes,
			&t.Relpersistence, &t.XIDAge, &last,
		); err != nil {
			return nil, 0, err
		}
		batch = append(batch, t)
	}
	return batch, last, rows.Err()
}

// collectIndexes pages through pg_stat_user_indexes by indexrelid (see
// collectTables; lifeos: 35,439 indexes).
func (c *Collector) collectIndexes(ctx context.Context) ([]IndexStats, error) {
	batchSize := collectorBatchSize(c.cfg)
	var result []IndexStats
	var cursor uint32
	for {
		batch, last, err := c.collectIndexBatch(ctx, cursor, batchSize)
		if err != nil {
			return nil, fmt.Errorf("collect indexes: %w", err)
		}
		result = append(result, batch...)
		if len(batch) < batchSize {
			return result, nil
		}
		cursor = last
	}
}

func (c *Collector) collectIndexBatch(
	ctx context.Context, cursor uint32, batchSize int,
) ([]IndexStats, uint32, error) {
	rows, err := c.catalogQuery(ctx, indexStatsSQL, cursor, batchSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var result []IndexStats
	var last uint32
	for rows.Next() {
		var idx IndexStats
		if err := rows.Scan(
			&idx.SchemaName, &idx.RelName, &idx.IndexRelName,
			&idx.IdxScan, &idx.IdxTupRead, &idx.IdxTupFetch,
			&idx.IndexBytes,
			&idx.IsUnique, &idx.IsPrimary, &idx.IsValid,
			&idx.IndexDef, &idx.IndexType, &last,
		); err != nil {
			return nil, 0, err
		}
		idx.IndexRelID = last
		result = append(result, idx)
	}
	return result, last, rows.Err()
}

func (c *Collector) collectForeignKeys(ctx context.Context) ([]ForeignKey, error) {
	rows, err := c.catalogQuery(ctx, foreignKeysSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ForeignKey
	for rows.Next() {
		var fk ForeignKey
		if err := rows.Scan(
			&fk.TableName, &fk.ReferencedTable,
			&fk.FKColumn, &fk.ConstraintName,
		); err != nil {
			return nil, err
		}
		result = append(result, fk)
	}
	return result, rows.Err()
}

func (c *Collector) collectSystem(ctx context.Context) (SystemStats, error) {
	sql := systemStatsSQL14
	if c.pgVersionNum >= 170000 {
		sql = systemStatsSQL17
	}
	s, err := c.scanSystemStats(ctx, sql)
	var pgErr *pgconn.PgError
	if c.pgVersionNum >= 170000 && errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		s, err = c.scanSystemStats(ctx, systemStatsSQL14)
	}
	return s, err
}

// scanSystemStats reads one system stats row. A NULL cache hit ratio (no
// block accesses yet) becomes CacheHitRatioUnknown, never 0 (G1-B08).
func (c *Collector) scanSystemStats(ctx context.Context, sql string) (SystemStats, error) {
	var s SystemStats
	var ratio *float64
	err := c.catalogQueryRow(ctx, sql).Scan(
		&s.ActiveBackends, &s.IdleInTransaction,
		&s.TotalBackends, &s.MaxConnections,
		&ratio, &s.Deadlocks,
		&s.BlkReadTime, &s.BlkWriteTime,
		&s.TotalCheckpoints, &s.IsReplica,
		&s.DBSizeBytes,
	)
	s.CacheHitRatio = CacheHitRatioUnknown
	if ratio != nil {
		s.CacheHitRatio = *ratio
	}
	if err == nil {
		s.RelationStatsEpoch = c.collectRelationStatsEpoch(ctx)
	}
	return s, err
}

func (c *Collector) collectLocks(ctx context.Context) ([]LockInfo, error) {
	rows, err := c.catalogQuery(ctx, locksSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []LockInfo
	for rows.Next() {
		var lk LockInfo
		if err := rows.Scan(
			&lk.LockType, &lk.Mode, &lk.Granted,
			&lk.RelName,
			&lk.Query, &lk.State,
			&lk.WaitEventType, &lk.WaitEvent,
			&lk.PID,
			&lk.BackendStart, &lk.QueryStart,
		); err != nil {
			return nil, err
		}
		result = append(result, lk)
	}
	return result, rows.Err()
}

// Sequences kept per snapshot: every used sequence at or above
// SequenceFloorPct of its range, plus the SequenceTopN most used, at most
// SequenceMaxRows. The 75% exhaustion rule and the forecaster (which
// ignores sequences under 1%) see every sequence that can matter to them.
const (
	SequenceFloorPct = 1.0
	SequenceTopN     = 100
	SequenceMaxRows  = 1000
)

func (c *Collector) collectSequences(ctx context.Context) ([]SequenceStats, error) {
	rows, err := c.catalogQuery(ctx, sequencesSQL, SequenceFloorPct, SequenceTopN,
		SequenceMaxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []SequenceStats
	for rows.Next() {
		var seq SequenceStats
		if err := rows.Scan(
			&seq.SchemaName, &seq.SequenceName, &seq.DataType,
			&seq.LastValue, &seq.MinValue, &seq.MaxValue,
			&seq.IncrementBy, &seq.Cycle, &seq.PctUsed,
		); err != nil {
			return nil, err
		}
		result = append(result, seq)
	}
	return result, rows.Err()
}
