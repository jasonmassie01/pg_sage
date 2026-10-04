package probes

import "fmt"

// pg_stat views for the tool-calling investigator (roadmap 2.1): the
// current database's cumulative counters, the user tables with the most
// dead tuples and sequential scans, and the statements with the most
// execution time. Catalog probes like the others (typed, capped,
// read-only, never query text). pg_sage's own schema is left out of the
// table view. The statement view reads pg_stat_statements without its
// text (showtext => false): loading and matching the text of every entry
// took over a second at 50000 entries, past the probe timeout, so
// pg_sage's own statements cannot be told apart by their text here;
// own_role marks the statements run by pg_sage's own role instead.

// Stat view probes.
const (
	StatDatabase   ID = "stat_database"
	StatTables     ID = "stat_tables"
	StatStatements ID = "stat_statements"
)

// FamilyStats groups the pg_stat view probes.
const FamilyStats = "stat_views"

// statViewRows bounds the workload views to their top rows.
const statViewRows = 50

// statViews maps the investigator's view names to their probes.
var statViews = map[string]ID{"database": StatDatabase, "tables": StatTables,
	"statements": StatStatements}

// StatView is the probe of a named pg_stat view ("" when unknown).
func StatView(name string) ID { return statViews[name] }

// StatViews lists the view names.
func StatViews() []string { return []string{"database", "tables", "statements"} }

const statDatabaseSQL = `/* pg_sage sre:stat_database v1 */
SELECT d.datname AS database, d.numbackends::int8 AS numbackends,
       d.xact_commit::int8 AS xact_commit, d.xact_rollback::int8 AS xact_rollback,
       d.blks_read::int8 AS blks_read, d.blks_hit::int8 AS blks_hit,
       d.tup_returned::int8 AS tup_returned, d.tup_fetched::int8 AS tup_fetched,
       d.tup_inserted::int8 AS tup_inserted, d.tup_updated::int8 AS tup_updated,
       d.tup_deleted::int8 AS tup_deleted, d.conflicts::int8 AS conflicts,
       d.temp_files::int8 AS temp_files, d.temp_bytes::int8 AS temp_bytes,
       d.deadlocks::int8 AS deadlocks,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - d.stats_reset)::float8
           AS stats_reset_age_s
FROM pg_catalog.pg_stat_database d
WHERE d.datname = pg_catalog.current_database()
LIMIT $1`

// statTablesSQL picks the top user tables first and names only those.
var statTablesSQL = `/* pg_sage sre:stat_tables v2 */
SELECT ` + fmt.Sprintf(relationName, "t.relid") + ` AS relation,
       t.seq_scan, t.idx_scan, t.n_live_tup, t.n_dead_tup, t.n_mod_since_analyze,
       t.vacuum_age_s, t.analyze_age_s, t.autovacuum_count
FROM (SELECT s.relid, s.seq_scan::int8 AS seq_scan,
             COALESCE(s.idx_scan, 0)::int8 AS idx_scan,
             s.n_live_tup::int8 AS n_live_tup, s.n_dead_tup::int8 AS n_dead_tup,
             s.n_mod_since_analyze::int8 AS n_mod_since_analyze,
             EXTRACT(EPOCH FROM pg_catalog.clock_timestamp()
                 - GREATEST(s.last_vacuum, s.last_autovacuum))::float8 AS vacuum_age_s,
             EXTRACT(EPOCH FROM pg_catalog.clock_timestamp()
                 - GREATEST(s.last_analyze, s.last_autoanalyze))::float8 AS analyze_age_s,
             s.autovacuum_count::int8 AS autovacuum_count
      FROM pg_catalog.pg_stat_user_tables s
      WHERE s.schemaname <> 'sage'
      ORDER BY s.n_dead_tup DESC, s.seq_scan DESC, s.relid
      LIMIT $1) t
ORDER BY t.n_dead_tup DESC, t.seq_scan DESC, t.relid`

// statStatementsSQL ranks this database's statements by execution time
// on pg_stat_statements' counters alone; v2 never loads query text.
const statStatementsSQL = `/* pg_sage sre:stat_statements v2 */
SELECT s.queryid, sum(s.calls)::int8 AS calls,
       sum(s.total_exec_time)::float8 AS total_exec_ms,
       (sum(s.total_exec_time) / GREATEST(sum(s.calls), 1))::float8 AS mean_exec_ms,
       sum(s.rows)::int8 AS rows,
       sum(s.shared_blks_hit)::int8 AS shared_blks_hit,
       sum(s.shared_blks_read)::int8 AS shared_blks_read,
       sum(s.temp_blks_written)::int8 AS temp_blks_written,
       bool_or(s.userid = (SELECT r.oid FROM pg_catalog.pg_roles r
                           WHERE r.rolname = current_user)) AS own_role
FROM ` + ExtSchemaToken + `.pg_stat_statements(showtext => false) s
WHERE s.dbid = (SELECT d.oid FROM pg_catalog.pg_database d
                WHERE d.datname = pg_catalog.current_database())
  AND s.queryid IS NOT NULL
GROUP BY s.queryid
ORDER BY 3 DESC, 1
LIMIT $1`

func statViewSpecs() []Spec {
	statements := needsStats(spec(StatStatements, FamilyStats, ArgsNone,
		Variant{MinVersion: 140000, SQL: statStatementsSQL}))
	statements.Extension = "pg_stat_statements"
	return []Spec{
		spec(StatDatabase, FamilyStats, ArgsNone,
			Variant{MinVersion: 140000, SQL: statDatabaseSQL}),
		capped(spec(StatTables, FamilyStats, ArgsNone,
			Variant{MinVersion: 140000, SQL: statTablesSQL}), statViewRows),
		capped(statements, statViewRows),
	}
}
