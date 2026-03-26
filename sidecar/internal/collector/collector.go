package collector

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// Collector runs periodic stats collection against the target database.
type Collector struct {
	pool         *pgxpool.Pool
	cfg          *config.Config
	breaker      *CircuitBreaker
	mu           sync.RWMutex
	latest       *Snapshot
	previous     *Snapshot
	tablePageKey string
	pgVersionNum int // e.g. 170009 for PG 17.9
	logFn        func(string, string, ...any)
}

// New creates a Collector wired to the given pool and config.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	pgVersionNum int,
	logFn func(string, string, ...any),
) *Collector {
	return &Collector{
		pool:         pool,
		cfg:          cfg,
		pgVersionNum: pgVersionNum,
		breaker: NewCircuitBreaker(
			cfg.Safety.CPUCeilingPct,
			cfg.Safety.BackoffConsecutiveSkips,
		),
		logFn: logFn,
	}
}

// Run starts the collection loop, blocking until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	interval := c.cfg.Collector.Interval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	c.logFn("INFO", "collector started, interval=%s", interval)

	for {
		select {
		case <-ctx.Done():
			c.logFn("INFO", "collector stopped")
			return
		case <-ticker.C:
			c.cycle(ctx, ticker)
		}
	}
}

func (c *Collector) cycle(ctx context.Context, ticker *time.Ticker) {
	if c.breaker.ShouldSkip(ctx, c.pool) {
		if c.breaker.IsDormant() {
			c.logFn("WARN", "circuit breaker dormant, using dormant interval")
			ticker.Reset(c.cfg.Safety.DormantInterval())
		} else {
			c.logFn("WARN", "circuit breaker skip, db load too high")
		}
		return
	}

	// Restore normal interval if we were dormant.
	if c.breaker.IsDormant() {
		ticker.Reset(c.cfg.Collector.Interval())
	}

	snap, err := c.collect(ctx)
	if err != nil {
		c.logFn("ERROR", "collection failed: %v", err)
		return
	}

	c.mu.Lock()
	c.previous = c.latest
	c.latest = snap
	c.mu.Unlock()

	c.breaker.RecordSuccess()

	if err := c.persist(ctx, snap); err != nil {
		c.logFn("ERROR", "snapshot persist failed: %v", err)
	}
}

// LatestSnapshot returns the most recent snapshot (thread-safe).
func (c *Collector) LatestSnapshot() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

// PreviousSnapshot returns the snapshot before the latest (thread-safe).
func (c *Collector) PreviousSnapshot() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.previous
}

// collect gathers all stats categories into a single Snapshot.
func (c *Collector) collect(ctx context.Context) (*Snapshot, error) {
	now := time.Now().UTC()
	snap := &Snapshot{CollectedAt: now}

	var err error

	if snap.Queries, err = c.collectQueries(ctx); err != nil {
		return nil, err
	}
	if snap.Tables, err = c.collectTables(ctx); err != nil {
		return nil, err
	}
	if snap.Indexes, err = c.collectIndexes(ctx); err != nil {
		return nil, err
	}
	if snap.ForeignKeys, err = c.collectForeignKeys(ctx); err != nil {
		return nil, err
	}
	if snap.System, err = c.collectSystem(ctx); err != nil {
		return nil, err
	}
	if snap.Locks, err = c.collectLocks(ctx); err != nil {
		return nil, err
	}
	if snap.Sequences, err = c.collectSequences(ctx); err != nil {
		return nil, err
	}
	if snap.Replication, err = c.collectReplication(ctx); err != nil {
		return nil, err
	}
	// pg_stat_io (PG16+)
	if c.pgVersionNum >= 160000 {
		if snap.IO, err = c.collectIO(ctx); err != nil {
			c.logFn("WARN", "pg_stat_io collection failed: %v", err)
			// Non-fatal — continue without IO stats.
		}
	}
	// Partition inheritance
	if snap.Partitions, err = c.collectPartitions(ctx); err != nil {
		c.logFn("WARN", "partition collection failed: %v", err)
	}
	// Config data for advisor features
	if c.cfg.Advisor.Enabled {
		if snap.ConfigData, err = collectConfigSnapshot(ctx, c.pool); err != nil {
			c.logFn("WARN", "config snapshot collection failed: %v", err)
		}
	}

	return snap, nil
}

func (c *Collector) collectQueries(ctx context.Context) ([]QueryStats, error) {
	sql := queryStatsSQL
	hasWAL := c.cfg.HasWALColumns
	hasPlan := c.cfg.HasPlanTimeColumns

	switch {
	case hasWAL && hasPlan:
		sql = queryStatsWithWALAndPlanTimeSQL
	case hasWAL:
		sql = queryStatsWithWALSQL
	case hasPlan:
		sql = queryStatsWithPlanTimeSQL
	}

	rows, err := c.pool.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []QueryStats
	for rows.Next() {
		var q QueryStats
		dest := []any{
			&q.QueryID, &q.Query, &q.Calls,
			&q.TotalExecTime, &q.MeanExecTime, &q.MinExecTime, &q.MaxExecTime,
			&q.StddevExecTime, &q.Rows,
			&q.SharedBlksHit, &q.SharedBlksRead,
			&q.SharedBlksDirtied, &q.SharedBlksWritten,
			&q.TempBlksRead, &q.TempBlksWritten,
			&q.BlkReadTime, &q.BlkWriteTime,
		}
		if hasWAL {
			dest = append(dest, &q.WALRecords, &q.WALFpi, &q.WALBytes)
		}
		if hasPlan {
			dest = append(dest, &q.TotalPlanTime, &q.MeanPlanTime)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		result = append(result, q)
	}
	return result, rows.Err()
}

func (c *Collector) collectTables(ctx context.Context) ([]TableStats, error) {
	batchSize := c.cfg.Collector.BatchSize
	var allTables []TableStats

	pageKey := c.tablePageKey
	for {
		rows, err := c.pool.Query(ctx, tableStatsSQL, pageKey, batchSize)
		if err != nil {
			return nil, err
		}

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
			); err != nil {
				rows.Close()
				return nil, err
			}
			batch = append(batch, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}

		allTables = append(allTables, batch...)

		if len(batch) < batchSize {
			// All tables collected; reset cursor for next cycle.
			c.tablePageKey = ""
			break
		}

		last := batch[len(batch)-1]
		pageKey = last.SchemaName + "." + last.RelName
		c.tablePageKey = pageKey
	}

	return allTables, nil
}

func (c *Collector) collectIndexes(ctx context.Context) ([]IndexStats, error) {
	rows, err := c.pool.Query(ctx, indexStatsSQL)
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
	rows, err := c.pool.Query(ctx, foreignKeysSQL)
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

	var s SystemStats
	err := c.pool.QueryRow(ctx, sql).Scan(
		&s.ActiveBackends, &s.IdleInTransaction,
		&s.TotalBackends, &s.MaxConnections,
		&s.CacheHitRatio, &s.Deadlocks,
		&s.TotalCheckpoints, &s.IsReplica,
		&s.DBSizeBytes,
	)
	return s, err
}

func (c *Collector) collectLocks(ctx context.Context) ([]LockInfo, error) {
	rows, err := c.pool.Query(ctx, locksSQL)
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
	rows, err := c.pool.Query(ctx, sequencesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []SequenceStats
	for rows.Next() {
		var seq SequenceStats
		if err := rows.Scan(
			&seq.SchemaName, &seq.SequenceName, &seq.DataType,
			&seq.LastValue, &seq.MaxValue, &seq.IncrementBy,
			&seq.PctUsed,
		); err != nil {
			return nil, err
		}
		result = append(result, seq)
	}
	return result, rows.Err()
}

func (c *Collector) collectReplication(
	ctx context.Context,
) (*ReplicationStats, error) {
	rs := &ReplicationStats{}

	// Collect replicas.
	replicaRows, err := c.pool.Query(ctx, replicationReplicasSQL)
	if err != nil {
		return nil, err
	}
	defer replicaRows.Close()

	for replicaRows.Next() {
		var r ReplicaInfo
		if err := replicaRows.Scan(
			&r.ClientAddr, &r.State,
			&r.SentLSN, &r.WriteLSN, &r.FlushLSN, &r.ReplayLSN,
			&r.WriteLag, &r.FlushLag, &r.ReplayLag,
			&r.SyncState,
		); err != nil {
			return nil, err
		}
		rs.Replicas = append(rs.Replicas, r)
	}
	if err := replicaRows.Err(); err != nil {
		return nil, err
	}

	// Collect slots.
	slotRows, err := c.pool.Query(ctx, replicationSlotsSQL)
	if err != nil {
		return nil, err
	}
	defer slotRows.Close()

	for slotRows.Next() {
		var s SlotInfo
		if err := slotRows.Scan(
			&s.SlotName, &s.SlotType, &s.Active,
			&s.RetainedBytes,
		); err != nil {
			return nil, err
		}
		rs.Slots = append(rs.Slots, s)
	}
	if err := slotRows.Err(); err != nil {
		return nil, err
	}

	// Return nil if no replication data exists.
	if len(rs.Replicas) == 0 && len(rs.Slots) == 0 {
		return nil, nil
	}

	return rs, nil
}

func (c *Collector) collectIO(ctx context.Context) ([]IOStats, error) {
	rows, err := c.pool.Query(ctx, ioStatsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []IOStats
	for rows.Next() {
		var s IOStats
		if err := rows.Scan(
			&s.BackendType, &s.Object, &s.Context,
			&s.Reads, &s.ReadTime,
			&s.Writes, &s.WriteTime,
			&s.Writebacks, &s.WritebackTime,
			&s.Extends, &s.ExtendTime,
			&s.Hits, &s.Evictions,
			&s.Reuses, &s.Fsyncs,
			&s.FsyncTime,
		); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

func (c *Collector) collectPartitions(
	ctx context.Context,
) ([]PartitionInfo, error) {
	rows, err := c.pool.Query(ctx, partitionInheritanceSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []PartitionInfo
	for rows.Next() {
		var p PartitionInfo
		if err := rows.Scan(
			&p.ChildTable, &p.ChildSchema,
			&p.ParentTable, &p.ParentSchema,
		); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// persist inserts the snapshot into sage.snapshots (one row per category).
func (c *Collector) persist(ctx context.Context, snap *Snapshot) error {
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
		if _, err := tx.Exec(ctx, insertSQL, snap.CollectedAt, cat, j); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}
