package collector

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/config"
)

// collectTables pages through pg_stat_user_tables with a keyset cursor
// that lives only for this call. The cursor used to persist on the
// Collector, so an error mid-pagination made the NEXT snapshot resume
// part-way and silently omit every table before it (G1-B18).
func (c *Collector) collectTables(ctx context.Context) ([]TableStats, error) {
	batchSize := c.cfg.Collector.BatchSize
	var allTables []TableStats

	// Tuple cursor: (schema, rel). Must be two separate bind params so
	// that PostgreSQL compares tuple-wise. Concatenating into a single
	// string silently skips tables: e.g. ('public','users') produces
	// cursor 'public.users', and 'public' < 'public.users' causes every
	// subsequent row in the public schema to be filtered out.
	var pageSchema, pageRel string
	for {
		batch, err := c.collectTableBatch(ctx, pageSchema, pageRel, batchSize)
		if err != nil {
			return nil, err
		}
		allTables = append(allTables, batch...)

		// Empty batch → nothing more to page through. This also
		// guards against an endless loop when batchSize is 0.
		if len(batch) == 0 || len(batch) < batchSize {
			return allTables, nil
		}
		last := batch[len(batch)-1]
		pageSchema, pageRel = last.SchemaName, last.RelName
	}
}

func (c *Collector) collectTableBatch(
	ctx context.Context, pageSchema, pageRel string, batchSize int,
) ([]TableStats, error) {
	rows, err := c.catalogQuery(ctx, tableStatsSQL, pageSchema, pageRel, batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var batch []TableStats
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
			&t.Relpersistence, &t.XIDAge,
		); err != nil {
			return nil, err
		}
		batch = append(batch, t)
	}
	return batch, rows.Err()
}

func (c *Collector) collectIndexes(ctx context.Context) ([]IndexStats, error) {
	batchSize := c.cfg.Collector.BatchSize
	if batchSize <= 0 {
		batchSize = config.DefaultCollectorBatchSize
	}
	var result []IndexStats
	var schemaName, tableName, indexName string
	for {
		batch, err := c.collectIndexBatch(ctx, schemaName, tableName, indexName, batchSize)
		if err != nil {
			return nil, fmt.Errorf("collect indexes: %w", err)
		}
		result = append(result, batch...)
		if len(batch) < batchSize {
			return result, nil
		}
		last := batch[len(batch)-1]
		schemaName, tableName, indexName = last.SchemaName, last.RelName, last.IndexRelName
	}
}

func (c *Collector) collectIndexBatch(
	ctx context.Context, schemaName, tableName, indexName string, batchSize int,
) ([]IndexStats, error) {
	rows, err := c.catalogQuery(ctx, indexStatsSQL, schemaName, tableName, indexName, batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []IndexStats
	for rows.Next() {
		var idx IndexStats
		if err := rows.Scan(
			&idx.SchemaName, &idx.RelName, &idx.IndexRelName,
			&idx.IdxScan, &idx.IdxTupRead, &idx.IdxTupFetch,
			&idx.IndexBytes,
			&idx.IsUnique, &idx.IsPrimary, &idx.IsValid,
			&idx.IndexDef, &idx.IndexType,
		); err != nil {
			return nil, err
		}
		result = append(result, idx)
	}
	return result, rows.Err()
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

func (c *Collector) collectSequences(ctx context.Context) ([]SequenceStats, error) {
	rows, err := c.catalogQuery(ctx, sequencesSQL)
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
