package migration

import (
	"context"
	"fmt"
)

const tableStatsSQL = `
SELECT COALESCE(c.reltuples, 0)::bigint,
       COALESCE(c.relpages, 0)::bigint * current_setting('block_size')::bigint
FROM   pg_class c
JOIN   pg_namespace n ON n.oid = c.relnamespace
WHERE  c.relname = $1
  AND  n.nspname = $2`

func (ra *RiskAssessor) fetchTableStats(
	ctx context.Context, risk *DDLRisk,
) {
	schema := schemaOrPublic(risk.SchemaName)
	row := ra.pool.QueryRow(ctx, tableStatsSQL, risk.TableName, schema)
	if err := row.Scan(&risk.EstimatedRows, &risk.TableSizeBytes); err != nil {
		ra.logFn("debug",
			"migration: table stats unavailable for %s.%s: %v",
			schema, risk.TableName, err)
	}
}

const activeQueriesSQL = `
SELECT COUNT(*)::int,
       COALESCE(MAX(EXTRACT(EPOCH FROM now() - query_start)), 0)
FROM   pg_stat_activity
WHERE  state = 'active'
  AND  query ~* $1`

func (ra *RiskAssessor) fetchActiveQueries(
	ctx context.Context, risk *DDLRisk,
) {
	pattern := fmt.Sprintf(`\b%s\b`, risk.TableName)
	row := ra.pool.QueryRow(ctx, activeQueriesSQL, pattern)
	if err := row.Scan(&risk.ActiveQueries, &risk.LongestQuerySec); err != nil {
		ra.logFn("debug",
			"migration: active query check failed for %s: %v",
			risk.TableName, err)
	}
}

const pendingLocksSQL = `
SELECT COUNT(*)::int
FROM   pg_locks l
JOIN   pg_class c ON c.oid = l.relation
JOIN   pg_namespace n ON n.oid = c.relnamespace
WHERE  NOT l.granted
  AND  c.relname = $1
  AND  n.nspname = $2`

func (ra *RiskAssessor) fetchPendingLocks(
	ctx context.Context, risk *DDLRisk,
) {
	schema := schemaOrPublic(risk.SchemaName)
	row := ra.pool.QueryRow(ctx, pendingLocksSQL, risk.TableName, schema)
	if err := row.Scan(&risk.PendingLocks); err != nil {
		ra.logFn("debug",
			"migration: pending lock check failed for %s.%s: %v",
			schema, risk.TableName, err)
	}
}

const replicationLagSQL = `
SELECT COALESCE(
  (SELECT MAX(EXTRACT(EPOCH FROM replay_lag)) FROM pg_stat_replication),
  0
)`

func (ra *RiskAssessor) fetchReplicationLag(
	ctx context.Context, risk *DDLRisk,
) {
	row := ra.pool.QueryRow(ctx, replicationLagSQL)
	if err := row.Scan(&risk.ReplicationLag); err != nil {
		ra.logFn("debug",
			"migration: replication lag query failed: %v", err)
	}
}

func schemaOrPublic(s string) string {
	if s == "" {
		return "public"
	}
	return s
}
