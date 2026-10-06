package schema

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// History store (history.store: meta, v2.3.0). The meta database keeps the
// telemetry history of every monitored database in its own sage.snapshots
// and sage.query_store, each row tagged with the database's meta-db record
// id (never reused, unlike a name). Only the meta database gets this:
// monitored databases' schemas are unchanged. Every step checks the
// catalog first or is IF NOT EXISTS, so a second run writes nothing.
//
//   - database_id on both tables: a catalog-only ALTER on the parent that
//     reaches every partition; partition.Convert keeps it (LIKE).
//   - indexes leading with database_id for every history read, so one
//     database's reads never scan the others' rows.
//   - sage.history_store_databases: which databases keep history here and
//     their last known size (the snapshot cap is the sum of their caps).
//   - the migration progress and id-map tables (EnsureHistoryMigrationTables).

// ErrNoSageSchema reports a database without the sage history tables.
var ErrNoSageSchema = errors.New("schema: the sage schema is not bootstrapped " +
	"(sage.snapshots and sage.query_store are missing)")

// historyStoreTimeout bounds the store bootstrap: the indexes are built on
// whatever history the meta database already holds.
const historyStoreTimeout = 5 * time.Minute

// historyTablesWithIdentity get the database_id column.
var historyTablesWithIdentity = []string{"snapshots", "query_store"}

const ddlHistoryStoreIndexes = `
CREATE INDEX IF NOT EXISTS idx_snapshots_db_category
    ON sage.snapshots (database_id, category, collected_at DESC);
CREATE INDEX IF NOT EXISTS idx_query_store_db_qid_time
    ON sage.query_store (database_id, queryid, captured_at DESC);
CREATE INDEX IF NOT EXISTS idx_query_store_db_time
    ON sage.query_store (database_id, captured_at DESC);
CREATE TABLE IF NOT EXISTS sage.history_store_databases (
    database_id   integer PRIMARY KEY,
    database_name text NOT NULL,
    db_bytes      bigint,
    registered_at timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
`

// ddlHistoryMigration holds a migration's progress and the snapshot id map
// in the destination: the progress row commits with each batch it
// describes, so a run resumes exactly where the last one stopped.
const ddlHistoryMigration = `
CREATE TABLE IF NOT EXISTS sage.history_migration (
    database_id  integer NOT NULL,
    direction    text NOT NULL CHECK (direction IN ('meta', 'monitored')),
    table_name   text NOT NULL CHECK (table_name IN ('snapshots', 'query_store')),
    last_id      bigint NOT NULL DEFAULT 0,
    last_at      timestamptz,
    copied       bigint NOT NULL DEFAULT 0,
    skipped      bigint NOT NULL DEFAULT 0,
    started_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    PRIMARY KEY (database_id, direction, table_name)
);
CREATE TABLE IF NOT EXISTS sage.history_migration_ids (
    database_id integer NOT NULL,
    direction   text NOT NULL,
    source_id   bigint NOT NULL,
    dest_id     bigint NOT NULL,
    PRIMARY KEY (database_id, direction, source_id)
);
`

// Execer runs one statement (a pool, connection or transaction).
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// EnsureHistoryMigrationTables creates the migration progress and id-map
// tables. It runs in the destination of a history migration, a meta or a
// monitored database.
func EnsureHistoryMigrationTables(ctx context.Context, db Execer) error {
	if _, err := db.Exec(ctx, ddlHistoryMigration); err != nil {
		return fmt.Errorf("create history migration tables: %w", err)
	}
	return nil
}

// BootstrapHistoryStore makes the meta database a history store. It runs
// after Bootstrap, only when history.store is meta, under the bootstrap
// advisory lock.
func BootstrapHistoryStore(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("history store bootstrap requires a PostgreSQL connection pool")
	}
	ctx, cancel := context.WithTimeout(ctx, historyStoreTimeout)
	defer cancel()
	return withAdvisoryLock(ctx, pool, bootstrapLockTimeout,
		func(conn *pgxpool.Conn) error { return bootstrapHistoryStore(ctx, conn) })
}

func bootstrapHistoryStore(ctx context.Context, db bootstrapDB) error {
	for _, table := range historyTablesWithIdentity {
		present, has, err := identityColumn(ctx, db, table)
		if err != nil {
			return err
		}
		if !present {
			return ErrNoSageSchema
		}
		if has {
			continue
		}
		// Checked first: ALTER TABLE takes ACCESS EXCLUSIVE on every run.
		if _, err := db.Exec(ctx, "ALTER TABLE sage."+table+
			" ADD COLUMN IF NOT EXISTS database_id integer"); err != nil {
			return fmt.Errorf("add sage.%s.database_id: %w", table, err)
		}
	}
	if _, err := db.Exec(ctx, ddlHistoryStoreIndexes); err != nil {
		return fmt.Errorf("history store indexes: %w", err)
	}
	return EnsureHistoryMigrationTables(ctx, db)
}

// identityColumn reports whether sage.<table> exists and has database_id.
func identityColumn(ctx context.Context, db bootstrapDB, table string) (bool, bool, error) {
	var present, has bool
	err := db.QueryRow(ctx, `SELECT to_regclass('sage.' || $1) IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_catalog.pg_attribute
		        WHERE attrelid = to_regclass('sage.' || $1)
		          AND attname = 'database_id' AND NOT attisdropped)`, table).
		Scan(&present, &has)
	if err != nil {
		return false, false, fmt.Errorf("inspect sage.%s: %w", table, err)
	}
	return present, has, nil
}
