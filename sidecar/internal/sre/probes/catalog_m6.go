package probes

// M6 reactive-family probes. Checkpoint and temp-file counters are
// cumulative and compared across samples; live temp files, the standby's
// replay state and wait events are snapshots. Every probe is read-only
// and covers PostgreSQL 14 to 18.

const checkpointWAL = `
       w.wal_bytes::int8 AS wal_bytes, w.wal_records::int8 AS wal_records,
       w.wal_fpi::int8 AS wal_fpi, w.stats_reset AS wal_stats_reset,
       (SELECT s.setting::int8 * 1048576 FROM pg_catalog.pg_settings s
        WHERE s.name = 'max_wal_size') AS max_wal_size_bytes,
       (SELECT s.setting::int8 FROM pg_catalog.pg_settings s
        WHERE s.name = 'checkpoint_timeout') AS checkpoint_timeout_s,
       pg_catalog.current_setting('checkpoint_completion_target')::float8
           AS checkpoint_completion_target,
       pg_catalog.pg_postmaster_start_time() AS server_started_at`

// Before PostgreSQL 17 the checkpointer's counters, and the writes and
// fsyncs backends had to do themselves, live in pg_stat_bgwriter.
const checkpointActivitySQL14 = `/* pg_sage sre:checkpoint_activity v1 */
SELECT b.checkpoints_timed::int8 AS timed_checkpoints,
       b.checkpoints_req::int8 AS requested_checkpoints,
       b.checkpoint_write_time::float8 AS checkpoint_write_ms,
       b.checkpoint_sync_time::float8 AS checkpoint_sync_ms,
       b.buffers_checkpoint::int8 AS buffers_written,
       b.buffers_backend::int8 AS backend_writes,
       b.buffers_backend_fsync::int8 AS backend_fsyncs,
       b.stats_reset AS checkpointer_stats_reset,` + checkpointWAL + `
FROM pg_catalog.pg_stat_bgwriter b CROSS JOIN pg_catalog.pg_stat_wal w
LIMIT $1`

// PostgreSQL 17 moved them to pg_stat_checkpointer and pg_stat_io.
const checkpointActivitySQL17 = `/* pg_sage sre:checkpoint_activity v1 */
SELECT c.num_timed::int8 AS timed_checkpoints,
       c.num_requested::int8 AS requested_checkpoints,
       c.write_time::float8 AS checkpoint_write_ms,
       c.sync_time::float8 AS checkpoint_sync_ms,
       c.buffers_written::int8 AS buffers_written,
       io.writes AS backend_writes, io.fsyncs AS backend_fsyncs,
       c.stats_reset AS checkpointer_stats_reset,` + checkpointWAL + `
FROM pg_catalog.pg_stat_checkpointer c CROSS JOIN pg_catalog.pg_stat_wal w
CROSS JOIN (SELECT sum(i.writes)::int8 AS writes, sum(i.fsyncs)::int8 AS fsyncs
            FROM pg_catalog.pg_stat_io i
            WHERE i.backend_type = 'client backend' AND i.object = 'relation') io
LIMIT $1`

const tempFileActivitySQL = `/* pg_sage sre:temp_file_activity v1 */
SELECT d.temp_files::int8 AS temp_files, d.temp_bytes::int8 AS temp_bytes,
       d.stats_reset,
       pg_catalog.pg_size_bytes(pg_catalog.current_setting('work_mem')) AS work_mem_bytes,
       pg_catalog.current_setting('hash_mem_multiplier')::float8 AS hash_mem_multiplier,
       (SELECT s.setting::int8 FROM pg_catalog.pg_settings s
        WHERE s.name = 'temp_file_limit') AS temp_file_limit_kb,
       pg_catalog.current_setting('temp_tablespaces') <> '' AS temp_tablespaces_set,
       pg_catalog.pg_postmaster_start_time() AS server_started_at
FROM pg_catalog.pg_stat_database d
WHERE d.datname = pg_catalog.current_database()
LIMIT $1`

// tempFileHoldersSQL attributes the live temp files of the default
// tablespace to backends by the pid in their names (pgsql_tmpPID.N).
// pg_ls_tmpdir needs pg_monitor (or superuser): without it the probe is
// no_privilege, never "no temp files".
const tempFileHoldersSQL = `/* pg_sage sre:temp_file_holders v1 */
WITH f AS (
    SELECT (pg_catalog.regexp_match(t.name, '^pgsql_tmp([0-9]+)\.'))[1]::int4 AS pid,
           t.size
    FROM pg_catalog.pg_ls_tmpdir() t
), g AS (
    SELECT f.pid, count(*)::int8 AS files, sum(f.size)::int8 AS bytes
    FROM f WHERE f.pid IS NOT NULL GROUP BY f.pid
), tot AS (SELECT COALESCE(sum(g.bytes), 0)::int8 AS total_bytes FROM g)
SELECT g.pid, a.backend_start,
       COALESCE(a.datname = pg_catalog.current_database(), false) AS in_current_database,
       COALESCE(a.state, 'gone') AS state, a.query_id,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - a.query_start)::float8
           AS query_age_s,
       g.files, g.bytes, tot.total_bytes
FROM g CROSS JOIN tot
LEFT JOIN pg_catalog.pg_stat_activity a ON a.pid = g.pid
ORDER BY g.bytes DESC, g.pid
LIMIT $1`

// tempSpillStatementsSQL reads this database's statements that wrote
// temp blocks, without their text (pg_stat_statements(false)).
const tempSpillStatementsSQL = `/* pg_sage sre:temp_spill_statements v1 */
SELECT s.queryid, sum(s.calls)::int8 AS calls,
       sum(s.temp_blks_written)::int8 AS temp_blks_written,
       sum(s.temp_blks_read)::int8 AS temp_blks_read,
       sum(s.total_exec_time)::float8 AS total_exec_ms,
       pg_catalog.current_setting('block_size')::int8 AS block_size,
       (SELECT i.stats_reset FROM ` + ExtSchemaToken + `.pg_stat_statements_info i)
           AS stats_reset
FROM ` + ExtSchemaToken + `.pg_stat_statements(false) s
WHERE s.dbid = (SELECT d.oid FROM pg_catalog.pg_database d
                WHERE d.datname = pg_catalog.current_database())
  AND s.queryid IS NOT NULL AND s.temp_blks_written > 0
GROUP BY s.queryid
ORDER BY 3 DESC, 1
LIMIT $1`

// standbyReplayStateSQL is the standby's own view of replay; on a
// primary it says so (in_recovery false) and its standby fields are NULL.
const standbyReplayStateSQL = `/* pg_sage sre:standby_replay_state v1 */
SELECT pg_catalog.pg_is_in_recovery() AS in_recovery,
       CASE WHEN pg_catalog.pg_is_in_recovery()
            THEN pg_catalog.pg_is_wal_replay_paused() END AS replay_paused,
       pg_catalog.pg_wal_lsn_diff(pg_catalog.pg_last_wal_receive_lsn(),
           pg_catalog.pg_last_wal_replay_lsn())::int8 AS receive_replay_bytes,
       EXTRACT(EPOCH FROM pg_catalog.clock_timestamp()
           - pg_catalog.pg_last_xact_replay_timestamp())::float8 AS last_replay_age_s,
       (SELECT sum(c.confl_tablespace + c.confl_lock + c.confl_snapshot
                   + c.confl_bufferpin + c.confl_deadlock)
        FROM pg_catalog.pg_stat_database_conflicts c)::int8 AS conflicts,
       (SELECT r.status FROM pg_catalog.pg_stat_wal_receiver r LIMIT 1)
           AS receiver_status,
       (SELECT s.setting::int8 FROM pg_catalog.pg_settings s
        WHERE s.name = 'max_standby_streaming_delay') AS max_standby_streaming_delay_ms,
       (SELECT max(EXTRACT(EPOCH FROM pg_catalog.clock_timestamp() - a.xact_start))
        FROM pg_catalog.pg_stat_activity a
        WHERE a.backend_type = 'client backend' AND a.state <> 'idle'
          AND a.pid <> pg_catalog.pg_backend_pid())::float8 AS longest_query_s,
       pg_catalog.pg_postmaster_start_time() AS server_started_at
LIMIT $1`

// lwlockWaitsSQL is one sample of what active backends wait on (CPU when
// they wait on nothing), grouped by wait event and query id. Waits are
// cluster-wide resources; the database flag attributes queries.
const lwlockWaitsSQL = `/* pg_sage sre:lwlock_waits v1 */
WITH s AS (
    SELECT COALESCE(a.wait_event_type, 'CPU') AS wait_event_type,
           COALESCE(a.wait_event, '') AS wait_event, a.query_id,
           COALESCE(a.datname = pg_catalog.current_database(), false)
               AS in_current_database
    FROM pg_catalog.pg_stat_activity a
    WHERE a.state = 'active' AND a.pid <> pg_catalog.pg_backend_pid()
      AND a.backend_type IN ('client backend', 'parallel worker')
), t AS (SELECT count(*)::int8 AS active_backends FROM s)
SELECT s.wait_event_type, s.wait_event, s.query_id, s.in_current_database,
       count(*)::int8 AS backends, t.active_backends
FROM s CROSS JOIN t
GROUP BY s.wait_event_type, s.wait_event, s.query_id, s.in_current_database,
         t.active_backends
ORDER BY backends DESC, s.wait_event_type, s.wait_event, s.query_id
LIMIT $1`

func checkpointActivitySpec() Spec {
	return spec(CheckpointActivity, FamilyWAL, ArgsNone,
		Variant{MinVersion: 140000, SQL: checkpointActivitySQL14},
		Variant{MinVersion: 170000, SQL: checkpointActivitySQL17})
}

func tempFileActivitySpec() Spec {
	return spec(TempFileActivity, FamilyTempFiles, ArgsNone,
		Variant{MinVersion: 140000, SQL: tempFileActivitySQL})
}

func tempFileHoldersSpec() Spec {
	return spec(TempFileHolders, FamilyTempFiles, ArgsNone,
		Variant{MinVersion: 140000, SQL: tempFileHoldersSQL})
}

func tempSpillStatementsSpec() Spec {
	s := spec(TempSpillStatements, FamilyTempFiles, ArgsNone,
		Variant{MinVersion: 140000, SQL: tempSpillStatementsSQL})
	s.Extension = "pg_stat_statements"
	return s
}

func standbyReplayStateSpec() Spec {
	return spec(StandbyReplayState, FamilyReplication, ArgsNone,
		Variant{MinVersion: 140000, SQL: standbyReplayStateSQL})
}

func lwlockWaitsSpec() Spec {
	return spec(LWLockWaits, FamilyWaits, ArgsNone,
		Variant{MinVersion: 140000, SQL: lwlockWaitsSQL})
}
