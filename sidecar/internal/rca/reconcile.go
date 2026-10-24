package rca

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Legacy incident reconciliation (dogfood lifeos-1). Incidents written
// before identity keys existed (identity_key and often database_name
// empty) were never deduplicated: lifeos had 143 open ones for 13
// identities. At hydration, the engine that owns the store backfills the
// identity of its open legacy rows (adopting rows without a database, as
// hydration always has) and merges each identity's open rows into the
// earliest: occurrences summed, last detection the latest, severity the
// worst; the others resolve as merged. It touches open rows only, in
// bounded batches, under an advisory lock per database, and a second run
// changes nothing. It needs the engine's database name, so it runs at
// hydration rather than as a schema migration; the migration adds the
// partial index it relies on (idx_incidents_identity_open).

// ReconcileStats is what one reconciliation changed.
type ReconcileStats struct {
	Backfilled int64 // open rows given an identity key (and database)
	Merged     int64 // open duplicates resolved into their identity's earliest
}

const (
	reconcileBatch       = 1000
	reconcileGroupsBatch = 100
)

// identityKeySQL is identityKey (lifecycle.go) in SQL: sha256 of source,
// database, the byte-sorted signal ids and the first affected object,
// joined by U+001F.
const identityKeySQL = `pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(
    i.source || E'\x1f' || $1::text || E'\x1f' ||
    COALESCE((SELECT pg_catalog.string_agg(s, ',' ORDER BY s COLLATE "C")
              FROM pg_catalog.unnest(i.signal_ids) s), '') || E'\x1f' ||
    COALESCE(i.affected_objects[1], ''), 'UTF8')), 'hex')`

const backfillIdentitySQL = `/* pg_sage */
WITH b AS (
    SELECT id FROM sage.incidents
    WHERE resolved_at IS NULL AND identity_key IS NULL
      AND (database_name = $1 OR database_name IS NULL OR database_name = '')
    ORDER BY id LIMIT $2
)
UPDATE sage.incidents i SET database_name = $1, identity_key = ` + identityKeySQL + `
FROM b WHERE i.id = b.id AND i.resolved_at IS NULL`

// mergeDuplicatesSQL reads the open rows of at most $2 identities once,
// into s: one row per identity with its survivor (the earliest) and the
// merged values. Both updates join s, so a plan that rescans it costs at
// most $2 tuples per row. It used to join a per-row CTE: with statistics
// older than the backlog, the plan re-ran the grouping once per open row
// (25 million index reads for 5,000 duplicates of one identity) or
// rescanned that CTE once per row.
const mergeDuplicatesSQL = `/* pg_sage */
WITH s AS MATERIALIZED (
    SELECT identity_key,
           (array_agg(id ORDER BY detected_at, id))[1] AS survivor,
           sum(occurrence_count) AS total,
           max(COALESCE(last_detected_at, detected_at)) AS last_seen,
           max(CASE severity WHEN 'critical' THEN 3 WHEN 'warning' THEN 2 ELSE 1 END) AS sev,
           min(escalated_at) AS escalated
    FROM sage.incidents
    WHERE resolved_at IS NULL AND database_name = $1 AND identity_key IS NOT NULL
    GROUP BY identity_key HAVING count(*) > 1
    LIMIT $2
), keep AS (
    UPDATE sage.incidents i
    SET occurrence_count = LEAST(s.total, 2147483647)::int,
        last_detected_at = GREATEST(i.last_detected_at, s.last_seen),
        severity = CASE s.sev WHEN 3 THEN 'critical' WHEN 2 THEN 'warning' ELSE 'info' END,
        escalated_at = COALESCE(i.escalated_at, s.escalated)
    FROM s WHERE i.id = s.survivor
    RETURNING i.id
), gone AS (
    UPDATE sage.incidents i
    SET resolved_at = pg_catalog.now(), resolved_by = $3,
        resolution_reason = 'merged into ' || s.survivor::text ||
            ': duplicate open incident with the same identity'
    FROM s
    WHERE i.database_name = $1 AND i.identity_key = s.identity_key
      AND i.resolved_at IS NULL AND i.id <> s.survivor
    RETURNING i.id
)
SELECT (SELECT count(*) FROM keep), (SELECT count(*) FROM gone)`

// ReconcileOpenIncidents backfills identity keys of the database's open
// legacy incidents and merges open duplicates of one identity. Safe to
// run concurrently and repeatedly.
func ReconcileOpenIncidents(ctx context.Context, pool *pgxpool.Pool,
	database string) (ReconcileStats, error) {
	var st ReconcileStats
	switch {
	case pool == nil:
		return st, errors.New("rca: reconcile requires a pool")
	case database == "":
		return st, errors.New("rca: reconcile requires a database name")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return st, fmt.Errorf("rca: reconcile open incidents: acquire: %w", err)
	}
	defer conn.Release()
	const lockSQL = `SELECT pg_catalog.pg_advisory_lock(pg_catalog.hashtext(
		'sage.incidents reconcile:' || $1))`
	if _, err := conn.Exec(ctx, lockSQL, database); err != nil {
		return st, fmt.Errorf("rca: reconcile open incidents: lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_catalog.pg_advisory_unlock(
			pg_catalog.hashtext('sage.incidents reconcile:' || $1))`, database)
	}()
	if st.Backfilled, err = backfillIdentities(ctx, conn, database); err != nil {
		return st, err
	}
	st.Merged, err = mergeDuplicates(ctx, conn, database)
	return st, err
}

func backfillIdentities(ctx context.Context, conn *pgxpool.Conn,
	database string) (int64, error) {
	var total int64
	for {
		tag, err := conn.Exec(ctx, backfillIdentitySQL, database, reconcileBatch)
		if err != nil {
			return total, fmt.Errorf("rca: reconcile: backfill identity keys: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < reconcileBatch {
			return total, nil
		}
	}
}

func mergeDuplicates(ctx context.Context, conn *pgxpool.Conn,
	database string) (int64, error) {
	var total int64
	for {
		var kept, merged int64
		err := conn.QueryRow(ctx, mergeDuplicatesSQL, database, reconcileGroupsBatch,
			ResolvedByMerged).Scan(&kept, &merged)
		if err != nil {
			return total, fmt.Errorf("rca: reconcile: merge duplicates: %w", err)
		}
		total += merged
		if kept < reconcileGroupsBatch {
			return total, nil
		}
	}
}

// reconcileLegacy runs ReconcileOpenIncidents before hydration loads the
// open incidents. A failure is logged and hydration continues with the
// rows as they are (duplicates stay open, as before).
func (e *Engine) reconcileLegacy(ctx context.Context, pool *pgxpool.Pool, name string) {
	if name == "" {
		return
	}
	st, err := ReconcileOpenIncidents(ctx, pool, name)
	if err != nil {
		e.logFn("warn", "rca: reconciling legacy open incidents for %q failed; "+
			"loading them unmerged: %v", name, err)
		return
	}
	if st != (ReconcileStats{}) {
		e.logFn("info", "rca: reconciled open incidents for %q: %d given an identity, "+
			"%d duplicates merged", name, st.Backfilled, st.Merged)
	}
}
