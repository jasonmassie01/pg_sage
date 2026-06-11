package retention

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

const batchSize = 1000

// Cleaner performs periodic data retention cleanup.
type Cleaner struct {
	pool  *pgxpool.Pool
	cfg   *config.Config
	logFn func(string, string, ...any)
}

// New creates a new retention Cleaner.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	logFn func(string, string, ...any),
) *Cleaner {
	return &Cleaner{pool: pool, cfg: cfg, logFn: logFn}
}

// Run performs batched deletes of expired data from all sage tables.
func (c *Cleaner) Run(ctx context.Context) {
	c.purgeTable(ctx, "snapshots", "collected_at",
		c.cfg.Retention.SnapshotsDays, "")
	c.purgeTable(ctx, "findings", "last_seen",
		c.cfg.Retention.FindingsDays, "AND status = 'resolved'")
	c.purgeTable(ctx, "action_log", "executed_at",
		c.cfg.Retention.ActionsDays, "")
	c.purgeTable(ctx, "explain_cache", "captured_at",
		c.cfg.Retention.ExplainsDays, "")
	// Keep the per-queryid query store (F2) bounded using the snapshots
	// retention window — it samples every cycle and would grow unbounded.
	c.purgeTable(ctx, "query_store", "captured_at",
		c.cfg.Retention.SnapshotsDays, "")
	c.cleanStaleFirstSeen(ctx)
}

// purgeTable deletes expired rows in batches of 1000 until none remain.
func (c *Cleaner) purgeTable(
	ctx context.Context,
	table string,
	timeCol string,
	retentionDays int,
	extraWhere string,
) {
	if retentionDays <= 0 {
		return
	}

	totalDeleted := int64(0)
	for {
		query := fmt.Sprintf(
			`DELETE FROM sage.%s
			 WHERE ctid IN (
				 SELECT ctid FROM sage.%s
				 WHERE %s < now() - make_interval(days => $1)
				 %s
				 LIMIT %d
			 )`,
			table, table, timeCol, extraWhere, batchSize,
		)

		tag, err := c.pool.Exec(ctx, query, retentionDays)
		if err != nil {
			c.logFn("retention",
				"error purging sage.%s: %v", table, err,
			)
			return
		}

		deleted := tag.RowsAffected()
		totalDeleted += deleted

		if deleted < batchSize {
			break
		}
	}

	if totalDeleted > 0 {
		c.logFn("retention",
			"purged %d rows from sage.%s (retention: %d days)",
			totalDeleted, table, retentionDays,
		)
	}
}

// cleanStaleFirstSeen removes first_seen:* entries from sage.config
// for indexes that no longer exist in the latest snapshot.
func (c *Cleaner) cleanStaleFirstSeen(ctx context.Context) {
	// Collect all first_seen keys from config.
	rows, err := c.pool.Query(ctx,
		`/* pg_sage */ SELECT key FROM sage.config WHERE key LIKE 'first_seen:%'`,
	)
	if err != nil {
		c.logFn("retention",
			"error reading first_seen keys: %v", err,
		)
		return
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			continue
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		c.logFn("retention",
			"error iterating first_seen keys: %v", err,
		)
		return
	}

	if len(keys) == 0 {
		return
	}

	// Get current index names from the latest snapshot.
	currentIndexes := make(map[string]bool)
	idxRows, err := c.pool.Query(ctx,
		`/* pg_sage */ SELECT DISTINCT data->>'indexrelname'
		 FROM sage.snapshots
		 WHERE category = 'indexes'
		   AND collected_at = (
			   SELECT max(collected_at) FROM sage.snapshots
			   WHERE category = 'indexes'
		   )`,
	)
	if err != nil {
		c.logFn("retention",
			"error reading latest index snapshot: %v", err,
		)
		return
	}
	defer idxRows.Close()

	for idxRows.Next() {
		var name string
		if err := idxRows.Scan(&name); err != nil {
			continue
		}
		currentIndexes["first_seen:"+name] = true
	}

	// Delete config entries for indexes no longer present. first_seen:*
	// keys are orphaned legacy data (nothing writes or reads them), so
	// removing them when the index snapshot is empty is harmless cleanup,
	// which is this function's purpose (the D1 audit "danger" was benign).
	deleted := 0
	for _, key := range keys {
		if currentIndexes[key] {
			continue
		}
		_, err := c.pool.Exec(ctx,
			"DELETE FROM sage.config WHERE key = $1", key,
		)
		if err != nil {
			c.logFn("retention",
				"error deleting stale config key %s: %v", key, err,
			)
			continue
		}
		deleted++
	}

	if deleted > 0 {
		c.logFn("retention",
			"cleaned %d stale first_seen entries from sage.config",
			deleted,
		)
	}
}
