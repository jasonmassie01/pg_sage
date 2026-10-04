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

// reloptionSQL reads one reloption of the relation aliased %[1]s as text
// (NULL when unset); only a relation with reloptions is looked into.
const reloptionSQL = `CASE WHEN %[1]s.reloptions IS NOT NULL THEN (
               SELECT x.option_value FROM pg_catalog.pg_options_to_table(%[1]s.reloptions) x
               WHERE x.option_name = '%[2]s' LIMIT 1) END`

// wraparoundTablesSQL (v2) ranks tables by the share of their effective
// freeze maximum (the table's reloption when it is lower than the
// setting) that their XID or multixact age has used. It ranks from
// pg_class alone and reads statistics, autovacuum_enabled and vacuum
// progress only for the tables it returns: v1 joined the statistics view
// for every relation of the catalog to return 50 (measured.md M8: 118 ms,
// ~78 times an hour on lifeos). Reloption values are cast only when they
// name the option read.
var wraparoundTablesSQL = `/* pg_sage sre:wraparound_tables v2 */
WITH g AS (
    SELECT pg_catalog.current_setting('autovacuum_freeze_max_age')::int8 AS xid_max,
           pg_catalog.current_setting('autovacuum_multixact_freeze_max_age')::int8
               AS mxid_max
), t AS (
    SELECT c.oid, c.reloptions, pg_catalog.age(c.relfrozenxid)::int8 AS xid_age,
           pg_catalog.mxid_age(c.relminmxid)::int8 AS mxid_age,
           LEAST(g.xid_max, COALESCE((` + fmt.Sprintf(reloptionSQL, "c",
	"autovacuum_freeze_max_age") + `)::int8, g.xid_max)) AS freeze_max_age,
           LEAST(g.mxid_max, COALESCE((` + fmt.Sprintf(reloptionSQL, "c",
	"autovacuum_multixact_freeze_max_age") + `)::int8, g.mxid_max)) AS mxid_freeze_max_age
    FROM pg_catalog.pg_class c CROSS JOIN g
    WHERE c.relkind IN ('r', 'm', 't') AND c.relfrozenxid <> '0'::xid
), top AS (
    SELECT t.*, GREATEST(t.xid_age::float8 / NULLIF(t.freeze_max_age, 0),
                         t.mxid_age::float8 / NULLIF(t.mxid_freeze_max_age, 0)) AS used
    FROM t
    ORDER BY used DESC NULLS LAST, t.oid
    LIMIT $1
)
SELECT ` + fmt.Sprintf(relationName, "top.oid") + ` AS relation,
       top.xid_age, top.mxid_age, top.freeze_max_age, top.mxid_freeze_max_age,
       COALESCE(pg_catalog.lower(` + fmt.Sprintf(reloptionSQL, "top",
	"autovacuum_enabled") + `) NOT IN ('false', 'off', 'no', '0'), true)
           AS autovacuum_enabled,
       pg_catalog.pg_stat_get_dead_tuples(top.oid)::int8 AS n_dead_tup,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp()
           - GREATEST(pg_catalog.pg_stat_get_last_autovacuum_time(top.oid),
                      pg_catalog.pg_stat_get_last_vacuum_time(top.oid)))::float8
           AS last_vacuum_age_s,
       pg_catalog.pg_stat_get_autovacuum_count(top.oid)::int8 AS autovacuum_count,
       EXISTS (SELECT 1 FROM pg_catalog.pg_stat_progress_vacuum p
               WHERE p.relid = top.oid AND p.datname = pg_catalog.current_database())
           AS vacuum_running
FROM top
ORDER BY top.used DESC NULLS LAST, top.oid`

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
  AND i.last_detected_at > pg_catalog.now()
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

// runwayTrendsSQL (v2) regresses each sampled series over the window:
// only its current epoch (a counter reset or a recreated object starts a
// new one), by its monotonic counter when it has one, else by its value.
// Series are walked in (kind, subject) order by a skip scan of
// runway_samples_series_idx and each reads only its own window, so a call
// reads the samples of the series it returns and nothing is sorted (v1
// sorted every sample of every series to return 200: 108 ms at 150,000
// samples). The recursion yields the series in index order, which is the
// order returned.
const runwayTrendsSQL = `/* pg_sage sre:runway_trends v2 */
WITH RECURSIVE series AS (
    (SELECT r.kind, r.subject FROM sage.runway_samples r
     ORDER BY r.kind, r.subject LIMIT 1)
    UNION ALL
    SELECT n.kind, n.subject FROM series s
    CROSS JOIN LATERAL (
        SELECT r.kind, r.subject FROM sage.runway_samples r
        WHERE (r.kind, r.subject) > (s.kind, s.subject)
        ORDER BY r.kind, r.subject LIMIT 1) n
)
SELECT s.kind, s.subject, a.samples, a.first_at, c.last_at, c.last_value, c.last_limit,
       a.rate_per_s, a.r2
FROM series s
CROSS JOIN LATERAL (
    SELECT r.epoch, r.sampled_at AS last_at, r.value AS last_value,
           r.limit_value AS last_limit
    FROM sage.runway_samples r
    WHERE r.kind = s.kind AND r.subject = s.subject
      AND r.sampled_at >= pg_catalog.now() - pg_catalog.make_interval(secs => $2)
    ORDER BY r.sampled_at DESC LIMIT 1) c
CROSS JOIN LATERAL (
    SELECT count(*)::int8 AS samples, min(r.sampled_at) AS first_at,
           pg_catalog.regr_slope(COALESCE(r.counter, r.value),
               EXTRACT(EPOCH FROM r.sampled_at)::float8) AS rate_per_s,
           pg_catalog.regr_r2(COALESCE(r.counter, r.value),
               EXTRACT(EPOCH FROM r.sampled_at)::float8) AS r2
    FROM sage.runway_samples r
    WHERE r.kind = s.kind AND r.subject = s.subject AND r.epoch = c.epoch
      AND r.sampled_at >= pg_catalog.now() - pg_catalog.make_interval(secs => $2)) a
LIMIT $1`

// runwaySpecs are the M6 runway probes.
func runwaySpecs() []Spec {
	return []Spec{
		needsStats(spec(XIDRunwayProbe, FamilyVacuum, ArgsNone,
			Variant{MinVersion: 140000, SQL: xidRunwaySQL})),
		background(versioned(capped(spec(WraparoundTablesProbe, FamilyVacuum, ArgsNone,
			Variant{MinVersion: 140000, SQL: wraparoundTablesSQL}), 50), "v2")),
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
		sequenceRunwaySpec(),
		versioned(capped(spec(RunwayTrendsProbe, FamilyRunway, ArgsWindow,
			Variant{MinVersion: 140000, SQL: runwayTrendsSQL}), 200), "v2"),
	}
}

// background gives a spec the background budget for the runway monitor's
// sampling (RunBackground); investigations keep StatementTimeout.
func background(s Spec) Spec {
	s.BackgroundTimeout = MaxBackgroundStatementTimeout
	return s
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
