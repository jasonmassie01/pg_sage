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

// ddlQueryRollupDaily (evid-01b) creates the daily tier of the per-queryid
// rollup: per queryid and UTC day, the sum of the day's hourly rows, how
// many hours had a row (hours) or an epoch break, and how many distinct
// plan fingerprints the day's hours carried. The hourly tier is kept 35
// days and the daily tier 400 (retention.query_rollup_*_days); the hourly
// coverage rows are kept as long as the daily tier and summed on read.
const ddlQueryRollupDaily = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.query_rollup_daily (
    database_id       integer,
    queryid           bigint NOT NULL,
    day               date NOT NULL,
    calls             bigint NOT NULL CHECK (calls >= 0),
    exec_ms           float8 NOT NULL CHECK (exec_ms >= 0),
    rows              bigint NOT NULL CHECK (rows >= 0),
    shared_blks_hit   bigint NOT NULL CHECK (shared_blks_hit >= 0),
    shared_blks_read  bigint NOT NULL CHECK (shared_blks_read >= 0),
    temp_blks_read    bigint NOT NULL CHECK (temp_blks_read >= 0),
    temp_blks_written bigint NOT NULL CHECK (temp_blks_written >= 0),
    wal_bytes         bigint CHECK (wal_bytes IS NULL OR wal_bytes >= 0),
    hours             smallint NOT NULL CHECK (hours BETWEEN 0 AND 24),
    epoch_breaks      smallint NOT NULL CHECK (epoch_breaks BETWEEN 0 AND 24),
    plan_hashes       smallint NOT NULL CHECK (plan_hashes >= 0),
    last_plan_hash    text,
    built_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS query_rollup_daily_key
    ON sage.query_rollup_daily (COALESCE(database_id, 0), queryid, day);
CREATE INDEX IF NOT EXISTS query_rollup_daily_age ON sage.query_rollup_daily (day);
`

// historyMigrationTables is the table_name check of sage.history_migration:
// the copied history tables and the derived rollup tables (evid-01b).
const historyMigrationTables = `CHECK (table_name IN ('snapshots', 'query_store',
    'query_rollup_daily', 'query_rollup_coverage'))`

// ddlHistoryMigrationRollupTables (evid-01b) replaces the table_name check
// of a sage.history_migration created before the derived rollup tables
// moved with a migration. The table exists only where a migration ran, so
// the step does nothing without it; the old check's name is read from
// pg_constraint. It also runs in EnsureHistoryMigrationTables.
const ddlHistoryMigrationRollupTables = `
SET LOCAL lock_timeout = '5s';
DO $$
DECLARE c record;
BEGIN
    IF to_regclass('sage.history_migration') IS NULL THEN
        RETURN;
    END IF;
    FOR c IN SELECT conname FROM pg_catalog.pg_constraint
        WHERE conrelid = 'sage.history_migration'::regclass AND contype = 'c'
          AND pg_catalog.pg_get_constraintdef(oid) LIKE '%table_name%'
          AND pg_catalog.pg_get_constraintdef(oid) NOT LIKE '%query_rollup_daily%'
    LOOP
        EXECUTE format('ALTER TABLE sage.history_migration DROP CONSTRAINT %I',
            c.conname);
    END LOOP;
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
        WHERE conrelid = 'sage.history_migration'::regclass AND contype = 'c'
          AND pg_catalog.pg_get_constraintdef(oid) LIKE '%query_rollup_daily%') THEN
        ALTER TABLE sage.history_migration ADD CONSTRAINT history_migration_table_name_check
            ` + historyMigrationTables + `;
    END IF;
END $$;
`
