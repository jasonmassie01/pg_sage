package probes

import "fmt"

// M6 runway probes (AI-SRE-SPEC §4 R2): the XID and multixact runway,
// per-table freeze horizons against each table's effective maximum, the
// holders of the xmin horizon, logged autovacuum cancellations, WAL
// position and settings, the WAL directory, sequences against their
// binding limit, and the trends the runway monitor sampled.
const (
	XIDRunwayProbe          ID = "xid_runway"
	WraparoundTablesProbe   ID = "wraparound_tables"
	XminHorizon             ID = "xmin_horizon"
	AutovacuumCancellations ID = "autovacuum_cancellations"
	WALRunwayProbe          ID = "wal_runway"
	WALDirectoryProbe       ID = "wal_directory"
	// ClusterDatabaseSizeProbe sums the databases' sizes: one cluster-level
	// measurement fleet runtimes on one cluster share per pass.
	ClusterDatabaseSizeProbe ID = "cluster_database_size"
	SequenceRunwayProbe      ID = "sequence_runway"
	RunwayTrendsProbe        ID = "runway_trends"
)

// Runway probe families.
const (
	FamilySequences = "sequence_exhaustion"
	FamilyRunway    = "runway"
)

const xidRunwaySQL = `/* pg_sage sre:xid_runway v1 */
WITH d AS (
    SELECT d.datname, pg_catalog.age(d.datfrozenxid)::int8 AS xid_age,
           pg_catalog.mxid_age(d.datminmxid)::int8 AS mxid_age
    FROM pg_catalog.pg_database d
)
SELECT pg_catalog.pg_snapshot_xmax(pg_catalog.pg_current_snapshot())::text::int8
           AS next_xid,
       (SELECT (c.datminmxid::text::int8 + pg_catalog.mxid_age(c.datminmxid))
               % 4294967296
        FROM pg_catalog.pg_database c
        WHERE c.datname = pg_catalog.current_database()) AS mxid_counter,
       (SELECT max(d.xid_age) FROM d) AS cluster_xid_age,
       (SELECT max(d.mxid_age) FROM d) AS cluster_mxid_age,
       (SELECT d.xid_age FROM d WHERE d.datname = pg_catalog.current_database())
           AS database_xid_age,
       (SELECT pg_catalog.left(d.datname::text, 64) FROM d
        ORDER BY d.xid_age DESC, d.datname LIMIT 1) AS oldest_database,
       pg_catalog.current_setting('autovacuum_freeze_max_age')::int8 AS freeze_max_age,
       pg_catalog.current_setting('autovacuum_multixact_freeze_max_age')::int8
           AS multixact_freeze_max_age,
       pg_catalog.current_setting('autovacuum') = 'on' AS autovacuum_on,
       pg_catalog.current_setting('autovacuum_max_workers')::int8
           AS autovacuum_max_workers,
       (SELECT count(*)::int8 FROM pg_catalog.pg_stat_activity a
        WHERE a.backend_type = 'autovacuum worker') AS autovacuum_workers
LIMIT $1`

// wraparoundTablesSQL ranks tables by the share of their effective freeze
// maximum (the table's reloption when it is lower than the setting) that
// their XID or multixact age has used. Reloption values are read in a
// CASE so only the named option is ever cast.
var wraparoundTablesSQL = `/* pg_sage sre:wraparound_tables v1 */
WITH g AS (
    SELECT pg_catalog.current_setting('autovacuum_freeze_max_age')::int8 AS xid_max,
           pg_catalog.current_setting('autovacuum_multixact_freeze_max_age')::int8
               AS mxid_max
), t AS (
    SELECT c.oid, pg_catalog.age(c.relfrozenxid)::int8 AS xid_age,
           pg_catalog.mxid_age(c.relminmxid)::int8 AS mxid_age,
           LEAST(g.xid_max, COALESCE(o.xid_max, g.xid_max)) AS freeze_max_age,
           LEAST(g.mxid_max, COALESCE(o.mxid_max, g.mxid_max)) AS mxid_freeze_max_age,
           COALESCE(o.enabled, true) AS autovacuum_enabled
    FROM pg_catalog.pg_class c CROSS JOIN g
    LEFT JOIN LATERAL (
        SELECT max(CASE WHEN x.option_name = 'autovacuum_freeze_max_age'
                        THEN x.option_value::int8 END) AS xid_max,
               max(CASE WHEN x.option_name = 'autovacuum_multixact_freeze_max_age'
                        THEN x.option_value::int8 END) AS mxid_max,
               bool_and(CASE WHEN x.option_name = 'autovacuum_enabled'
                        THEN pg_catalog.lower(x.option_value) NOT IN
                             ('false', 'off', 'no', '0') END) AS enabled
        FROM pg_catalog.pg_options_to_table(c.reloptions) x
    ) o ON true
    WHERE c.relkind IN ('r', 'm', 't') AND c.relfrozenxid <> '0'::xid
)
SELECT ` + fmt.Sprintf(relationName, "t.oid") + ` AS relation,
       t.xid_age, t.mxid_age, t.freeze_max_age, t.mxid_freeze_max_age,
       t.autovacuum_enabled, s.n_dead_tup::int8 AS n_dead_tup,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp()
           - GREATEST(s.last_autovacuum, s.last_vacuum))::float8 AS last_vacuum_age_s,
       s.autovacuum_count::int8 AS autovacuum_count,
       EXISTS (SELECT 1 FROM pg_catalog.pg_stat_progress_vacuum p
               WHERE p.relid = t.oid AND p.datname = pg_catalog.current_database())
           AS vacuum_running
FROM t LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = t.oid
ORDER BY GREATEST(t.xid_age::float8 / NULLIF(t.freeze_max_age, 0),
                  t.mxid_age::float8 / NULLIF(t.mxid_freeze_max_age, 0)) DESC NULLS LAST,
         t.oid
LIMIT $1`

// xminHorizonSQL lists what holds back the xmin horizon of this
// database's tables: its client sessions (their snapshot or XID),
// standbys' feedback through walsenders, its prepared transactions, and
// replication slots (xmin, and catalog_xmin for the catalogs). Prepared
// transactions are named by the hash of their gid. The probe's own
// session is never listed.
const xminHorizonSQL = `/* pg_sage sre:xmin_horizon v1 */
WITH h AS (
    SELECT CASE WHEN a.backend_type = 'walsender' THEN 'standby' ELSE 'session' END
               AS holder_kind,
           a.pid::int8 AS pid, a.backend_start, NULL::text AS holder_name,
           COALESCE(a.state, 'unknown') AS state,
           GREATEST(pg_catalog.age(a.backend_xmin), pg_catalog.age(a.backend_xid))::int8
               AS xmin_age,
           EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - a.xact_start)::float8
               AS xact_age_s,
           pg_catalog.left(COALESCE(a.application_name, ''), 64) AS application_name,
           NULL::bool AS active
    FROM pg_catalog.pg_stat_activity a
    WHERE (a.backend_xmin IS NOT NULL OR a.backend_xid IS NOT NULL)
      AND a.pid <> pg_catalog.pg_backend_pid()
      AND ((a.backend_type = 'client backend'
            AND a.datname = pg_catalog.current_database())
           OR a.backend_type = 'walsender')
    UNION ALL
    SELECT 'prepared_xact', NULL, NULL, pg_catalog.md5(p.gid), 'prepared',
           pg_catalog.age(p.transaction)::int8,
           EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - p.prepared)::float8,
           NULL, NULL
    FROM pg_catalog.pg_prepared_xacts p
    WHERE p.database = pg_catalog.current_database()
    UNION ALL
    SELECT 'slot', NULL, NULL, pg_catalog.left(s.slot_name::text, 64),
           CASE WHEN s.active THEN 'active' ELSE 'inactive' END,
           pg_catalog.age(s.xmin)::int8, NULL, NULL, s.active
    FROM pg_catalog.pg_replication_slots s WHERE s.xmin IS NOT NULL
    UNION ALL
    SELECT 'slot_catalog', NULL, NULL, pg_catalog.left(s.slot_name::text, 64),
           CASE WHEN s.active THEN 'active' ELSE 'inactive' END,
           pg_catalog.age(s.catalog_xmin)::int8, NULL, NULL, s.active
    FROM pg_catalog.pg_replication_slots s WHERE s.catalog_xmin IS NOT NULL
)
SELECT h.holder_kind, h.pid, h.backend_start, h.holder_name, h.state, h.xmin_age,
       h.xact_age_s, h.application_name, h.active
FROM h
ORDER BY h.xmin_age DESC NULLS LAST, h.holder_kind, h.pid, h.holder_name
LIMIT $1`

// autovacuumCancellationsSQL counts the RCA incidents raised from
// "canceling autovacuum task" log lines in the window. It reads pg_sage's
// own incident store, so a database without a log source shows none.
const autovacuumCancellationsSQL = `/* pg_sage sre:autovacuum_cancellations v1 */
SELECT count(*)::int8 AS cancel_incidents
FROM sage.incidents i
WHERE 'log_autovacuum_cancel' = ANY (i.signal_ids)
  AND i.last_detected_at > pg_catalog.clock_timestamp()
      - pg_catalog.make_interval(secs => $2)
LIMIT $1`

// walRunwaySQL (v2) reads the WAL position (the replayed one on a
// standby), the WAL size settings in bytes, and which cluster and role
// answered, so fleet runtimes on one cluster can share the cluster-level
// database size (cluster_database_size) instead of each summing it.
const walRunwaySQL = `/* pg_sage sre:wal_runway v2 */
SELECT pg_catalog.pg_wal_lsn_diff(` + currentLSN + `, '0/0')::float8
           AS wal_position_bytes,
       pg_catalog.pg_size_bytes(pg_catalog.current_setting('max_wal_size'))
           AS max_wal_size_bytes,
       pg_catalog.pg_size_bytes(pg_catalog.current_setting('wal_keep_size'))
           AS wal_keep_size_bytes,
       pg_catalog.pg_size_bytes(pg_catalog.current_setting('max_slot_wal_keep_size'))
           AS max_slot_wal_keep_size_bytes,
       pg_catalog.pg_size_bytes(pg_catalog.current_setting('wal_segment_size'))
           AS wal_segment_size_bytes,
       pg_catalog.pg_is_in_recovery() AS in_recovery,
       (SELECT c.system_identifier::text FROM pg_catalog.pg_control_system() c)
           AS system_identifier,
       pg_catalog.pg_postmaster_start_time() AS server_started_at,
       current_user::text AS role_name
LIMIT $1`

// clusterDatabaseSizeSQL is the size of every database this role may
// connect to, and how many it may not.
const clusterDatabaseSizeSQL = `/* pg_sage sre:cluster_database_size v1 */
SELECT (SELECT sum(pg_catalog.pg_database_size(d.oid))::int8
        FROM pg_catalog.pg_database d
        WHERE d.datallowconn AND pg_catalog.has_database_privilege(d.oid, 'CONNECT'))
           AS database_bytes,
       (SELECT count(*)::int8 FROM pg_catalog.pg_database d
        WHERE d.datallowconn
          AND NOT pg_catalog.has_database_privilege(d.oid, 'CONNECT'))
           AS databases_unreadable
LIMIT $1`

// walDirectorySQL sizes pg_wal and counts segments waiting for the
// archiver. Both functions need pg_monitor (or superuser).
const walDirectorySQL = `/* pg_sage sre:wal_directory v1 */
SELECT (SELECT COALESCE(sum(w.size), 0)::int8 FROM pg_catalog.pg_ls_waldir() w)
           AS wal_dir_bytes,
       (SELECT count(*)::int8 FROM pg_catalog.pg_ls_waldir() w) AS wal_files,
       (SELECT count(*)::int8 FROM pg_catalog.pg_ls_archive_statusdir() a
        WHERE a.name LIKE '%.ready') AS archive_ready_files
LIMIT $1`

// sequenceRunwaySQL ranks ascending sequences by the share of their
// effective limit used: the lower of the sequence's maximum and the
// maximum of the integer column that owns it. Descending sequences are
// not covered.
var sequenceRunwaySQL = `/* pg_sage sre:sequence_runway v1 */
WITH s AS (
    SELECT q.seqrelid, pg_catalog.quote_ident(n.nspname) || '.' ||
               pg_catalog.quote_ident(c.relname) AS seq,
           pg_catalog.format_type(q.seqtypid, NULL) AS data_type,
           q.seqincrement AS increment_by, q.seqcycle AS cycle,
           q.seqmin AS min_value, q.seqmax AS max_value, v.last_value
    FROM pg_catalog.pg_sequence q
    JOIN pg_catalog.pg_class c ON c.oid = q.seqrelid
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    LEFT JOIN pg_catalog.pg_sequences v
        ON v.schemaname = n.nspname AND v.sequencename = c.relname
    WHERE q.seqincrement > 0
), o AS (
    SELECT DISTINCT ON (d.objid) d.objid AS seqrelid,
           ` + fmt.Sprintf(relationName, "d.refobjid") + ` || '.' ||
               pg_catalog.quote_ident(a.attname) AS owner_column,
           pg_catalog.format_type(a.atttypid, NULL) AS owner_type
    FROM pg_catalog.pg_depend d
    JOIN pg_catalog.pg_attribute a
        ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
    WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
      AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
      AND d.refobjsubid > 0 AND d.deptype IN ('a', 'i')
    ORDER BY d.objid, d.refobjid, d.refobjsubid
), x AS (
    SELECT s.*, o.owner_column, o.owner_type,
           CASE s.data_type WHEN 'smallint' THEN 32767 WHEN 'integer' THEN 2147483647
                ELSE 9223372036854775807 END::int8 AS type_max,
           CASE o.owner_type WHEN 'smallint' THEN 32767 WHEN 'integer' THEN 2147483647
                WHEN 'bigint' THEN 9223372036854775807 END::int8 AS owner_type_max
    FROM s LEFT JOIN o ON o.seqrelid = s.seqrelid
), y AS (
    SELECT x.*, LEAST(x.max_value, COALESCE(x.owner_type_max, x.max_value))
               AS effective_limit
    FROM x
)
SELECT y.seq AS sequence, y.data_type, y.increment_by, y.cycle, y.last_value,
       y.min_value, y.max_value, y.type_max, y.owner_column, y.owner_type,
       y.owner_type_max, y.effective_limit,
       (y.last_value::numeric - y.min_value)
           / NULLIF(y.effective_limit::numeric - y.min_value, 0) AS fraction_used
FROM y
ORDER BY fraction_used DESC NULLS LAST, y.seq
LIMIT $1`

// runwayTrendsSQL regresses each sampled series over the window: only
// its current epoch (a counter reset or a recreated object starts a new
// one), by its monotonic counter when it has one, else by its value.
const runwayTrendsSQL = `/* pg_sage sre:runway_trends v1 */
WITH s AS (
    SELECT r.kind, r.subject, r.epoch, r.sampled_at, r.value, r.counter, r.limit_value,
           EXTRACT(EPOCH FROM r.sampled_at)::float8 AS t
    FROM sage.runway_samples r
    WHERE r.sampled_at >= pg_catalog.clock_timestamp()
          - pg_catalog.make_interval(secs => $2)
), cur AS (
    SELECT DISTINCT ON (s.kind, s.subject) s.kind, s.subject, s.epoch,
           s.sampled_at AS last_at, s.value AS last_value, s.limit_value AS last_limit
    FROM s ORDER BY s.kind, s.subject, s.sampled_at DESC
)
SELECT c.kind, c.subject, count(*)::int8 AS samples, min(s.sampled_at) AS first_at,
       c.last_at, c.last_value, c.last_limit,
       pg_catalog.regr_slope(COALESCE(s.counter, s.value), s.t) AS rate_per_s,
       pg_catalog.regr_r2(COALESCE(s.counter, s.value), s.t) AS r2
FROM cur c
JOIN s ON s.kind = c.kind AND s.subject = c.subject AND s.epoch = c.epoch
GROUP BY c.kind, c.subject, c.last_at, c.last_value, c.last_limit
ORDER BY c.kind, c.subject
LIMIT $1`

// runwaySpecs are the M6 runway probes.
func runwaySpecs() []Spec {
	return []Spec{
		needsStats(spec(XIDRunwayProbe, FamilyVacuum, ArgsNone,
			Variant{MinVersion: 140000, SQL: xidRunwaySQL})),
		capped(spec(WraparoundTablesProbe, FamilyVacuum, ArgsNone,
			Variant{MinVersion: 140000, SQL: wraparoundTablesSQL}), 50),
		capped(needsStats(spec(XminHorizon, FamilyVacuum, ArgsNone,
			Variant{MinVersion: 140000, SQL: xminHorizonSQL})), 100),
		spec(AutovacuumCancellations, FamilyVacuum, ArgsWindow,
			Variant{MinVersion: 140000, SQL: autovacuumCancellationsSQL}),
		versioned(spec(WALRunwayProbe, FamilyWAL, ArgsNone,
			Variant{MinVersion: 140000, SQL: walRunwaySQL}), "v2"),
		spec(ClusterDatabaseSizeProbe, FamilyRunway, ArgsNone,
			Variant{MinVersion: 140000, SQL: clusterDatabaseSizeSQL}),
		spec(WALDirectoryProbe, FamilyWAL, ArgsNone,
			Variant{MinVersion: 140000, SQL: walDirectorySQL}),
		capped(spec(SequenceRunwayProbe, FamilySequences, ArgsNone,
			Variant{MinVersion: 140000, SQL: sequenceRunwaySQL}), 50),
		capped(spec(RunwayTrendsProbe, FamilyRunway, ArgsWindow,
			Variant{MinVersion: 140000, SQL: runwayTrendsSQL}), 200),
	}
}

// versioned sets a spec's version.
func versioned(s Spec, v string) Spec {
	s.Version = v
	return s
}

// capped lowers a spec's row cap.
func capped(s Spec, rows int) Spec {
	s.MaxRows = rows
	return s
}
