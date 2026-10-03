package migration

import (
	"context"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

const tableStatsSQL = `
SELECT c.reltuples::bigint,
       c.relpages::bigint * current_setting('block_size')::bigint
FROM   pg_class c
JOIN   pg_namespace n ON n.oid = c.relnamespace
WHERE  c.relname = $1
  AND  n.nspname = $2`

func (ra *RiskAssessor) fetchTableStats(
	ctx context.Context, risk *DDLRisk,
) {
	schema := schemaOrPublic(risk.SchemaName)
	row := ra.catalog().QueryRow(ctx, tableStatsSQL, risk.TableName, schema)
	if err := row.Scan(&risk.EstimatedRows, &risk.TableSizeBytes); err != nil {
		ra.logFn("debug",
			"migration: table stats unavailable for %s.%s: %v",
			schema, risk.TableName, err)
		return
	}
	// reltuples = -1 means "never analyzed" (PG14+): size unknown.
	risk.StatsKnown = risk.EstimatedRows >= 0
}

// activeQueriesSQL counts other active backends in THIS database whose
// query mentions the table as a whole word. PostgreSQL ARE uses \y for
// a word boundary; \b is a backspace (G7-B02). pg_sage's own sessions
// (probes, EXPLAINs and plan captures run application query text) are
// not load the DDL competes with.
var activeQueriesSQL = `
SELECT COUNT(*)::int,
       COALESCE(MAX(EXTRACT(EPOCH FROM now() - query_start)), 0)
FROM   pg_stat_activity
WHERE  state = 'active'
  AND  pid <> pg_backend_pid()
  AND  datname = current_database()
  AND  query ~* $1
  AND  ` + selfmonitor.ActivityExclusionSQL("")

func (ra *RiskAssessor) fetchActiveQueries(
	ctx context.Context, risk *DDLRisk,
) {
	pattern := `\y` + quoteARE(risk.TableName) + `\y`
	row := ra.catalog().QueryRow(ctx, activeQueriesSQL, pattern)
	if err := row.Scan(&risk.ActiveQueries, &risk.LongestQuerySec); err != nil {
		ra.logFn("debug",
			"migration: active query check failed for %s: %v",
			risk.TableName, err)
	}
}

var areSpecial = regexp.MustCompile(`[\\.^$|?*+()\[\]{}]`)

// quoteARE escapes regex metacharacters for a PostgreSQL ARE pattern.
func quoteARE(s string) string {
	return areSpecial.ReplaceAllStringFunc(s, func(m string) string {
		return `\` + m
	})
}

// pendingLocksSQL counts ungranted locks on the table in THIS database;
// pg_locks is cluster-wide and relation OIDs collide across databases
// (G7-B17). A pg_sage session waiting (under lock_timeout) is not an
// application waiter.
var pendingLocksSQL = `
SELECT COUNT(*)::int
FROM   pg_locks l
JOIN   pg_class c ON c.oid = l.relation
JOIN   pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_activity a ON a.pid = l.pid
WHERE  NOT l.granted
  AND  ` + selfmonitor.ActivityExclusionSQL("a") + `
  AND  l.database = (SELECT oid FROM pg_database
                      WHERE datname = current_database())
  AND  c.relname = $1
  AND  n.nspname = $2`

func (ra *RiskAssessor) fetchPendingLocks(
	ctx context.Context, risk *DDLRisk,
) {
	schema := schemaOrPublic(risk.SchemaName)
	row := ra.catalog().QueryRow(ctx, pendingLocksSQL, risk.TableName, schema)
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
	row := ra.catalog().QueryRow(ctx, replicationLagSQL)
	if err := row.Scan(&risk.ReplicationLag); err != nil {
		ra.logFn("debug",
			"migration: replication lag query failed: %v", err)
	}
}

func schemaOrPublic(s string) string {
	if strings.TrimSpace(s) == "" {
		return "public"
	}
	return s
}
