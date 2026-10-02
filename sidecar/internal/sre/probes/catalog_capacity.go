package probes

import "fmt"

// Connection, replication and WAL/checkpoint probes. Counts are
// cluster-wide where the limit is (max_connections, WAL, slots) and are
// labeled with whether they belong to the current database.

const connectionSaturationSQL = `/* pg_sage sre:connection_saturation v2 */
WITH c AS (
    SELECT a.datname = pg_catalog.current_database() AS in_current_database,
           pg_catalog.left(COALESCE(a.application_name, ''), 64) AS application_name,
           COALESCE(pg_catalog.host(a.client_addr), 'local') AS client_addr,
           COALESCE(a.state, 'unknown') AS state,
           COALESCE(a.wait_event_type = 'Lock', false) AS waiting
    FROM pg_catalog.pg_stat_activity a
    WHERE a.backend_type = 'client backend'
), t AS (SELECT count(*)::int8 AS total FROM c)
SELECT c.in_current_database, c.application_name, c.client_addr, c.state,
       count(*)::int8 AS backends,
       count(*) FILTER (WHERE c.waiting)::int8 AS waiting_on_lock,
       pg_catalog.current_setting('max_connections')::int8 AS max_connections,
       pg_catalog.current_setting('superuser_reserved_connections')::int8
           AS reserved_connections,
       t.total AS total_client_backends,
       pg_catalog.pg_postmaster_start_time() AS server_started_at
FROM c CROSS JOIN t
GROUP BY c.in_current_database, c.application_name, c.client_addr, c.state, t.total
ORDER BY backends DESC, c.application_name, c.client_addr, c.state
LIMIT $1`

// currentLSN is the local WAL position on a primary and the replayed
// position on a standby (pg_current_wal_lsn fails during recovery).
const currentLSN = `CASE WHEN pg_catalog.pg_is_in_recovery()
        THEN pg_catalog.pg_last_wal_replay_lsn()
        ELSE pg_catalog.pg_current_wal_lsn() END`

const replicationLagSQL = `/* pg_sage sre:replication_lag v1 */
SELECT pg_catalog.left(COALESCE(r.application_name, ''), 64) AS application_name,
       COALESCE(pg_catalog.host(r.client_addr), 'local') AS client_addr,
       r.state, r.sync_state,
       EXTRACT(EPOCH FROM r.write_lag)::float8 AS write_lag_s,
       EXTRACT(EPOCH FROM r.flush_lag)::float8 AS flush_lag_s,
       EXTRACT(EPOCH FROM r.replay_lag)::float8 AS replay_lag_s,
       pg_catalog.pg_wal_lsn_diff(` + currentLSN + `, r.replay_lsn)::int8
           AS replay_lag_bytes
FROM pg_catalog.pg_stat_replication r
ORDER BY replay_lag_bytes DESC NULLS LAST, application_name
LIMIT $1`

const replicationSlotsSQL = `/* pg_sage sre:replication_slots v1 */
SELECT s.slot_name::text AS slot_name, s.slot_type, s.active, s.wal_status,
       pg_catalog.pg_wal_lsn_diff(` + currentLSN + `, s.restart_lsn)::int8
           AS retained_bytes,
       s.safe_wal_size::int8 AS safe_wal_size,
       s.database::text AS database,
       %s AS inactive_since
FROM pg_catalog.pg_replication_slots s
ORDER BY retained_bytes DESC NULLS LAST, slot_name
LIMIT $1`

const walCheckpointSettings = `
       (SELECT setting::int8 FROM pg_catalog.pg_settings
        WHERE name = 'checkpoint_timeout') AS checkpoint_timeout_s,
       (SELECT setting::int8 FROM pg_catalog.pg_settings
        WHERE name = 'max_wal_size') AS max_wal_size_mb,`

// Before PostgreSQL 17 checkpoint counters live in pg_stat_bgwriter.
const walCheckpointSQL14 = `/* pg_sage sre:wal_checkpoint v1 */
SELECT b.checkpoints_timed::int8 AS timed_checkpoints,
       b.checkpoints_req::int8 AS requested_checkpoints,
       b.checkpoint_write_time::float8 AS checkpoint_write_ms,
       b.checkpoint_sync_time::float8 AS checkpoint_sync_ms,
       b.buffers_checkpoint::int8 AS buffers_written,
       w.wal_bytes::int8 AS wal_bytes,
       w.wal_buffers_full::int8 AS wal_buffers_full,` + walCheckpointSettings + `
       b.stats_reset AS checkpointer_stats_reset,
       w.stats_reset AS wal_stats_reset
FROM pg_catalog.pg_stat_bgwriter b CROSS JOIN pg_catalog.pg_stat_wal w
LIMIT $1`

// PostgreSQL 17 moved checkpoint counters to pg_stat_checkpointer.
const walCheckpointSQL17 = `/* pg_sage sre:wal_checkpoint v1 */
SELECT c.num_timed::int8 AS timed_checkpoints,
       c.num_requested::int8 AS requested_checkpoints,
       c.write_time::float8 AS checkpoint_write_ms,
       c.sync_time::float8 AS checkpoint_sync_ms,
       c.buffers_written::int8 AS buffers_written,
       w.wal_bytes::int8 AS wal_bytes,
       w.wal_buffers_full::int8 AS wal_buffers_full,` + walCheckpointSettings + `
       c.stats_reset AS checkpointer_stats_reset,
       w.stats_reset AS wal_stats_reset
FROM pg_catalog.pg_stat_checkpointer c CROSS JOIN pg_catalog.pg_stat_wal w
LIMIT $1`

// connectionSaturationSpec is v2: M2 added the server start time, so a
// restart between two samples invalidates their comparison.
func connectionSaturationSpec() Spec {
	s := needsStats(spec(ConnectionSaturation, FamilyConnections, ArgsNone,
		Variant{MinVersion: 140000, SQL: connectionSaturationSQL}))
	s.Version = "v2"
	return s
}

func replicationLagSpec() Spec {
	return needsStats(spec(ReplicationLag, FamilyReplication, ArgsNone,
		Variant{MinVersion: 140000, SQL: replicationLagSQL}))
}

func replicationSlotsSpec() Spec {
	return spec(ReplicationSlots, FamilyReplication, ArgsNone,
		Variant{MinVersion: 140000,
			SQL: fmt.Sprintf(replicationSlotsSQL, "NULL::timestamptz")},
		Variant{MinVersion: 170000,
			SQL: fmt.Sprintf(replicationSlotsSQL, "s.inactive_since")})
}

func walCheckpointSpec() Spec {
	return spec(WALCheckpoint, FamilyWAL, ArgsNone,
		Variant{MinVersion: 140000, SQL: walCheckpointSQL14},
		Variant{MinVersion: 170000, SQL: walCheckpointSQL17})
}
