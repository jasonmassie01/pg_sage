package schema

// ddlQueryRollup (evid-01a) creates the hourly per-queryid rollup of the
// snapshot queries category. sage.query_rollup_hourly holds, per queryid
// and UTC hour, the sum of the same-epoch differences between consecutive
// queries snapshots whose later member is in the hour; epoch_break marks
// an hour with a pair that crossed a statistics epoch (it counts nothing).
// sage.query_rollup_coverage holds one row per hour: what the rollup saw
// against the database's own statement totals.
//
// database_id is NULL in monitored mode and the meta-db record id in meta
// mode (history.store), so the unique keys COALESCE it. The tables are
// not partitioned: at the 500-queryid cap 90 days of hourly rows are about
// 1.1M rows per database, and the daily purge deletes about 12,000.
// Every counter is checked non-negative: a negative delta cannot be
// stored, whatever the builder does. The _age indexes serve retention's
// purge by hour across databases.
//
// CREATE ... IF NOT EXISTS on new tables and their indexes; lock_timeout
// bounds the catalog locks on the first run.
const ddlQueryRollup = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.query_rollup_hourly (
    database_id       integer,
    queryid           bigint NOT NULL,
    hour              timestamptz NOT NULL
                          CHECK (mod(extract(epoch FROM hour)::numeric, 3600) = 0),
    calls             bigint NOT NULL CHECK (calls >= 0),
    exec_ms           float8 NOT NULL CHECK (exec_ms >= 0),
    rows              bigint NOT NULL CHECK (rows >= 0),
    shared_blks_hit   bigint NOT NULL CHECK (shared_blks_hit >= 0),
    shared_blks_read  bigint NOT NULL CHECK (shared_blks_read >= 0),
    temp_blks_read    bigint NOT NULL CHECK (temp_blks_read >= 0),
    temp_blks_written bigint NOT NULL CHECK (temp_blks_written >= 0),
    wal_bytes         bigint CHECK (wal_bytes IS NULL OR wal_bytes >= 0),
    plan_hash         text,
    epoch_break       boolean NOT NULL,
    pairs             smallint NOT NULL CHECK (pairs >= 0),
    built_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS query_rollup_hourly_key
    ON sage.query_rollup_hourly (COALESCE(database_id, 0), queryid, hour);
CREATE INDEX IF NOT EXISTS query_rollup_hourly_hour
    ON sage.query_rollup_hourly (COALESCE(database_id, 0), hour);
CREATE TABLE IF NOT EXISTS sage.query_rollup_coverage (
    database_id       integer,
    hour              timestamptz NOT NULL
                          CHECK (mod(extract(epoch FROM hour)::numeric, 3600) = 0),
    db_exec_ms        float8 CHECK (db_exec_ms IS NULL OR db_exec_ms >= 0),
    sampled_exec_ms   float8 NOT NULL CHECK (sampled_exec_ms >= 0),
    sampled_queryids  integer NOT NULL CHECK (sampled_queryids >= 0),
    epoch_breaks      integer NOT NULL CHECK (epoch_breaks >= 0),
    dealloc_delta     bigint CHECK (dealloc_delta IS NULL OR dealloc_delta >= 0),
    snapshots         smallint NOT NULL CHECK (snapshots >= 0),
    epoch_source      text NOT NULL CHECK (epoch_source IN ('recorded', 'counters_only')),
    built_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS query_rollup_coverage_key
    ON sage.query_rollup_coverage (COALESCE(database_id, 0), hour);
CREATE INDEX IF NOT EXISTS query_rollup_hourly_age ON sage.query_rollup_hourly (hour);
CREATE INDEX IF NOT EXISTS query_rollup_coverage_age ON sage.query_rollup_coverage (hour);
`
