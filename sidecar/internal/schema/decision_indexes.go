package schema

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Performance indexes on the decision ledger (dogfood lifeos, perf audit
// F1/F8). Each is built CONCURRENTLY so writers are never blocked, which
// cannot run inside Bootstrap: CREATE INDEX CONCURRENTLY waits for every
// older snapshot, including another sidecar's session blocked on the
// bootstrap advisory lock (a deadlock), and may outlast the 30 s migration
// budget on a large ledger. The runtime ensures them in the background on a
// dedicated connection; concurrent callers are serialized by a
// non-blocking advisory lock; an INVALID index left by a failed build is
// rebuilt; an existing valid index is left alone (the coordinator created
// idx_decision_schema_guard_targets by hand on lifeos with this exact
// definition).
//
//   - idx_decision_schema_guard_targets: the schema guard's history lookup
//     by target (0.2 ms instead of a scan per invariant).
//   - idx_decision_created: retention's age-based purge (the only index
//     on created_at led with database_id, NULL on every lifeos row).
//   - the rest: every foreign key into or out of sage.decision, so purges
//     (ON DELETE SET NULL probes) and retention's keep anti-joins never
//     scan a table.

type decisionIndex struct{ name, table, columns string }

var decisionIndexes = []decisionIndex{
	{"idx_decision_schema_guard_targets", "decision",
		"USING gin (target_objects) WHERE feature = 'schema_guard'"},
	{"idx_decision_created", "decision", "(created_at)"},
	{"idx_decision_action_log", "decision",
		"(action_log_id) WHERE action_log_id IS NOT NULL"},
	{"idx_decision_queue", "decision", "(queue_id) WHERE queue_id IS NOT NULL"},
	{"idx_decision_policy", "decision", "(policy_id) WHERE policy_id IS NOT NULL"},
	{"idx_verification_decision", "verification", "(decision_id)"},
	{"idx_change_lease_decision", "change_lease", "(decision_id)"},
	{"idx_incident_avoided_decision", "incident_avoided", "(decision_id)"},
	{"idx_schema_baseline_decision", "schema_baseline",
		"(last_authorized_decision_id) WHERE last_authorized_decision_id IS NOT NULL"},
}

const (
	decisionIndexLockKey = "pg_sage_decision_indexes"
	decisionIndexUnlock  = 5 * time.Second
)

const decisionIndexStateSQL = `SELECT i.indisvalid, EXISTS (
	SELECT 1 FROM pg_stat_progress_create_index p WHERE p.index_relid = i.indexrelid)
FROM pg_index i
WHERE i.indexrelid = to_regclass('sage.' || $1)`

// EnsureDecisionIndexes creates each decision ledger index that is missing
// or invalid. It returns nil without waiting when another session is
// already ensuring them, and skips a table that does not exist yet.
func EnsureDecisionIndexes(ctx context.Context, pool *pgxpool.Pool) (err error) {
	if pool == nil {
		return errors.New("decision indexes require a database connection")
	}
	// A dedicated connection, not a pool slot: a concurrent build can wait a
	// long time for older transactions, and the runtime's pools are small.
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("connect for the decision indexes: %w", err)
	}
	defer func() { err = errors.Join(err, closeIndexConn(conn)) }()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1))",
		decisionIndexLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("lock decision index build: %w", err)
	}
	if !locked {
		return nil
	}
	var buildErr error
	for _, index := range decisionIndexes {
		if err := ensureDecisionIndex(ctx, conn, index); err != nil {
			buildErr = errors.Join(buildErr, err)
		}
	}
	return errors.Join(buildErr, unlockDecisionIndexes(conn))
}

func ensureDecisionIndex(ctx context.Context, conn *pgx.Conn, index decisionIndex) error {
	var table *string
	if err := conn.QueryRow(ctx, "SELECT to_regclass('sage.' || $1)::text",
		index.table).Scan(&table); err != nil {
		return fmt.Errorf("look up sage.%s: %w", index.table, err)
	}
	if table == nil {
		return nil
	}
	var valid, building bool
	err := conn.QueryRow(ctx, decisionIndexStateSQL, index.name).Scan(&valid, &building)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read %s state: %w", index.name, err)
	case valid || building:
		return nil
	default:
		if _, err := conn.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS sage."+
			index.name); err != nil {
			return fmt.Errorf("drop invalid %s: %w", index.name, err)
		}
	}
	ddl := "CREATE INDEX CONCURRENTLY IF NOT EXISTS " + index.name + " ON sage." +
		index.table + " " + index.columns
	if _, err := conn.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("create %s concurrently: %w", index.name, err)
	}
	return nil
}

// closeIndexConn ends the build session and with it any session lock.
func closeIndexConn(conn *pgx.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), decisionIndexUnlock)
	defer cancel()
	if err := conn.Close(ctx); err != nil {
		return fmt.Errorf("close decision index connection: %w", err)
	}
	return nil
}

// unlockDecisionIndexes releases the build lock. If that fails, closing the
// dedicated connection (deferred) releases it with the session.
func unlockDecisionIndexes(conn *pgx.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), decisionIndexUnlock)
	defer cancel()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtext($1))",
		decisionIndexLockKey); err != nil {
		return fmt.Errorf("unlock decision index build: %w", err)
	}
	return nil
}
