package collector

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// The single-statement queries read before collection-11, kept verbatim
// as the golden reference: the split read (counters, then cached texts)
// must return the same queries, order and counters.

const legacyQueryStatsWith = sageTag + `
WITH ranked AS (
    SELECT queryid FROM pg_stat_statements(false)
     WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
       AND queryid IS NOT NULL
     GROUP BY queryid
     ORDER BY sum(total_exec_time) DESC
     LIMIT 2 * %[3]d
), candidates AS MATERIALIZED (
    SELECT s.* FROM pg_stat_statements(true) s JOIN ranked r ON r.queryid = s.queryid
     WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
)`

const legacyQueryStatsSelect = legacyQueryStatsWith + `
SELECT COALESCE(queryid, 0) AS queryid,
       (array_agg(query ORDER BY calls DESC))[1] AS query,
       sum(calls)::bigint AS calls,
       sum(total_exec_time)::float8 AS total_exec_time,
       COALESCE(sum(total_exec_time) / NULLIF(sum(calls), 0), 0)::float8 AS mean_exec_time,
       COALESCE(min(min_exec_time), 0)::float8 AS min_exec_time,
       COALESCE(max(max_exec_time), 0)::float8 AS max_exec_time,
       COALESCE(sqrt(GREATEST(
         sum(calls * (stddev_exec_time ^ 2 + mean_exec_time ^ 2)) / NULLIF(sum(calls), 0)
         - (sum(total_exec_time) / NULLIF(sum(calls), 0)) ^ 2, 0)), 0)::float8
         AS stddev_exec_time,
       sum(rows)::bigint AS rows,
       sum(shared_blks_hit)::bigint, sum(shared_blks_read)::bigint,
       sum(shared_blks_dirtied)::bigint, sum(shared_blks_written)::bigint,
       sum(temp_blks_read)::bigint, sum(temp_blks_written)::bigint,
       COALESCE(sum(%[1]s), 0)::float8 AS blk_read_time,
       COALESCE(sum(%[2]s), 0)::float8 AS blk_write_time`

const legacyQueryStatsWALColumns = `,
       sum(wal_records)::bigint, sum(wal_fpi)::bigint, sum(wal_bytes)::bigint`

const legacyQueryStatsPlanColumns = `,
       COALESCE(sum(total_plan_time), 0)::float8 AS total_plan_time,
       COALESCE(sum(total_plan_time) / NULLIF(sum(plans), 0), 0)::float8
         AS mean_plan_time`

// legacyQueryStatsFrom leaves pg_sage's own statements out of the candidates (a
// format string: the predicate's % are doubled).
var legacyQueryStatsFrom = `
  FROM candidates
 WHERE ` + strings.ReplaceAll(selfmonitor.StatementExclusionSQL("query"), "%", "%%") + `
 GROUP BY queryid
 ORDER BY sum(total_exec_time) DESC
 LIMIT %d`

// legacyQueryStatsTemplate is the old queryStatsTemplate.
func legacyQueryStatsTemplate(hasWAL, hasPlan bool) string {
	sql := legacyQueryStatsSelect
	if hasWAL {
		sql += legacyQueryStatsWALColumns
	}
	if hasPlan {
		sql += legacyQueryStatsPlanColumns
	}
	return sql + legacyQueryStatsFrom
}
