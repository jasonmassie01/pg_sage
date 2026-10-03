package schema

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The schema guard reads its remediation history by target table
// (sage.decision rows of feature schema_guard whose target_objects contain
// a table). Without an index that lookup scanned the ledger once per
// invariant per cycle (dogfood lifeos: one core at ~85%); with this partial
// GIN index it is 0.2 ms. The coordinator created it by hand on lifeos with
// the same name and definition, so an existing index is left alone.
//
// The index is built CONCURRENTLY so writers are never blocked, which
// cannot run inside Bootstrap: CREATE INDEX CONCURRENTLY waits for every
// older snapshot, including another sidecar's session blocked on the
// bootstrap advisory lock (a deadlock), and may outlast the 30 s migration
// budget on a large ledger. The runtime ensures it in the background
// instead; concurrent callers are serialized by a non-blocking advisory
// lock, and an INVALID index left by a failed build is rebuilt.

const (
	schemaGuardIndexLockKey = "pg_sage_schema_guard_targets_index"
	schemaGuardIndexUnlock  = 5 * time.Second
)

const schemaGuardIndexStateSQL = `SELECT i.indisvalid, EXISTS (
	SELECT 1 FROM pg_stat_progress_create_index p WHERE p.index_relid = i.indexrelid)
FROM pg_index i
WHERE i.indexrelid = to_regclass('sage.idx_decision_schema_guard_targets')`

const ddlSchemaGuardIndex = `CREATE INDEX CONCURRENTLY IF NOT EXISTS
    idx_decision_schema_guard_targets ON sage.decision USING gin (target_objects)
    WHERE feature = 'schema_guard'`

const dropInvalidSchemaGuardIndex = `DROP INDEX CONCURRENTLY IF EXISTS
    sage.idx_decision_schema_guard_targets`

// EnsureSchemaGuardIndex creates the schema guard history index when it is
// missing or invalid. It returns nil without waiting when another session
// is already ensuring or building it.
func EnsureSchemaGuardIndex(ctx context.Context, pool *pgxpool.Pool) (err error) {
	if pool == nil {
		return errors.New("schema guard index requires a database connection")
	}
	// A dedicated connection, not a pool slot: a concurrent build can wait a
	// long time for older transactions, and the runtime's pools are small.
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("connect for the schema guard index: %w", err)
	}
	defer func() { err = errors.Join(err, closeIndexConn(conn)) }()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1))",
		schemaGuardIndexLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("lock schema guard index build: %w", err)
	}
	if !locked {
		return nil
	}
	buildErr := ensureSchemaGuardIndex(ctx, conn)
	return errors.Join(buildErr, unlockSchemaGuardIndex(conn))
}

// closeIndexConn ends the build session and with it any session lock.
func closeIndexConn(conn *pgx.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), schemaGuardIndexUnlock)
	defer cancel()
	if err := conn.Close(ctx); err != nil {
		return fmt.Errorf("close schema guard index connection: %w", err)
	}
	return nil
}

func ensureSchemaGuardIndex(ctx context.Context, conn *pgx.Conn) error {
	var valid, building bool
	err := conn.QueryRow(ctx, schemaGuardIndexStateSQL).Scan(&valid, &building)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read schema guard index state: %w", err)
	case valid || building:
		return nil
	default:
		if _, err := conn.Exec(ctx, dropInvalidSchemaGuardIndex); err != nil {
			return fmt.Errorf("drop invalid schema guard index: %w", err)
		}
	}
	if _, err := conn.Exec(ctx, ddlSchemaGuardIndex); err != nil {
		return fmt.Errorf("create schema guard index concurrently: %w", err)
	}
	return nil
}

// unlockSchemaGuardIndex releases the build lock. If that fails, closing
// the dedicated connection (deferred) releases it with the session.
func unlockSchemaGuardIndex(conn *pgx.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), schemaGuardIndexUnlock)
	defer cancel()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtext($1))",
		schemaGuardIndexLockKey); err != nil {
		return fmt.Errorf("unlock schema guard index build: %w", err)
	}
	return nil
}
