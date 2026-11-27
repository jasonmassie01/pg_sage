package schema

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/startup"
)

const (
	bootstrapAdvisoryLockKey = "pg_sage"
	bootstrapLockTimeout     = 30 * time.Second
	bootstrapUnlockTimeout   = 5 * time.Second
)

type bootstrapDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}

type advisoryLock struct {
	mu       sync.Mutex
	conn     *pgxpool.Conn
	released bool
	// discard closes the session on release instead of returning it to
	// the pool: its settings could not be reset (config-04).
	discard bool
}

// expectedTables lists every table the sage schema must contain.
var expectedTables = []struct {
	name string
	ddl  string
}{
	{"action_log", ddlActionLog},
	{"snapshots", ddlSnapshots},
	{"findings", ddlFindings},
	{"explain_cache", ddlExplainCache},
	{"briefings", ddlBriefings},
	{"config", ddlConfig},
	{"alert_log", ddlAlertLog},
	{"query_hints", ddlQueryHints},
	{"users", ddlUsers},
	{"sessions", ddlSessions},
	{"databases", ddlDatabases},
	{"notification_channels", ddlNotificationChannels},
	{"notification_rules", ddlNotificationRules},
	{"notification_log", ddlNotificationLog},
	{"action_queue", ddlActionQueue},
	{"incidents", ddlIncidents},
	{"explain_results", ddlExplainResults},
	{"schema_findings", ddlSchemaFindings},
	{"crypto_meta", ddlCryptoMeta},
	{"health_history", ddlHealthHistory},
	{"query_store", ddlQueryStore},
	{"recommendation", ddlRecommendation},
	{"recommendation_revision", ddlRecommendationRevision},
	{"recommendation_transition", ddlRecommendationTransition},
}

// Bootstrap acquires an advisory lock, then ensures the sage schema and
// all required tables exist. It never drops existing objects.
//
// v0.8.5 — Bootstrap now folds in MigrateConfigSchema as a final step so
// every caller receives a fully-migrated sage.config (with database_id /
// updated_by_user_id columns) and a live sage.config_audit table. Before
// this change, any code path that dropped + re-bootstrapped the schema
// (notably the schema-package tests) left sage.config_audit missing,
// which caused unrelated integration tests elsewhere to fail intermittently
// when run in parallel.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("schema bootstrap requires a PostgreSQL connection pool")
	}
	endpoint := pool.Config().ConnConfig
	if err := startup.ValidateSessionEndpoint(endpoint.Host, endpoint.Port); err != nil {
		return err
	}
	ctx = withMigrationLedger(ctx, &migrationLedger{}) // config-04b
	return withBootstrapSession(
		ctx, pool,
		func(conn *pgxpool.Conn) error {
			exists, err := schemaExists(ctx, conn)
			if err != nil {
				return fmt.Errorf("checking sage schema: %w", err)
			}

			if !exists {
				if err := createFullSchema(ctx, conn); err != nil {
					return err
				}
			} else if err := ensureTablesExist(ctx, conn); err != nil {
				return err
			}

			if err := migrateConfigSchema(ctx, conn); err != nil {
				return fmt.Errorf("config migration: %w", err)
			}
			if err := migrateIncidentConstraints(ctx, conn); err != nil {
				return fmt.Errorf("incident constraint migration: %w", err)
			}
			if err := migrateRetentionForeignKeys(ctx, conn); err != nil {
				return err
			}
			if err := migrateStorage(ctx, conn); err != nil { // storage_migration.go
				return err
			}
			if err := migrateAuditChain(ctx, conn); err != nil { // audit_chain_migration.go
				return err
			}
			if err := migrateSIEMCursorAnchor(ctx, conn); err != nil { // audit-01
				return err
			}
			if err := migrateLateSteps(ctx, conn); err != nil {
				return err
			}
			return ledgerFrom(ctx).seal(ctx, conn)
		},
	)
}

// migrateLateSteps runs the newest migrations, in order, after the rest.
func migrateLateSteps(ctx context.Context, conn bootstrapDB) error {
	if err := migrateLocksCleanup(ctx, conn); err != nil { // locks_cleanup_migration.go
		return err
	}
	if err := retireSequenceForecast(ctx, conn); err != nil { // forecast-05
		return err
	}
	if err := migrateAuditCheckpoint(ctx, conn); err != nil { // audit-03
		return err
	}
	if err := migrateUsersEmailCase(ctx, conn); err != nil { // apiauth-05
		return err
	}
	return migrateCryptoMetaKeyID(ctx, conn) // security-08
}

func acquireAdvisoryLock(
	ctx context.Context, pool *pgxpool.Pool, timeout time.Duration,
) (*advisoryLock, error) {
	// Use blocking pg_advisory_lock with a timeout instead of
	// pg_try_advisory_lock. This prevents spurious failures when
	// multiple sidecar instances or test packages start concurrently
	// — the lock is held only briefly during schema bootstrap, so
	// waiting up to 30 seconds is acceptable.
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := pool.Acquire(qctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring advisory-lock connection: %w", err)
	}

	_, err = conn.Exec(
		qctx,
		"SELECT pg_advisory_lock(hashtext($1))",
		bootstrapAdvisoryLockKey,
	)
	if err != nil {
		discardErr := discardPinnedConn(conn)
		return nil, errors.Join(fmt.Errorf(
			"advisory lock: %w (another pg_sage instance "+
				"may be bootstrapping)", err,
		), discardErr)
	}
	return &advisoryLock{conn: conn}, nil
}

func withAdvisoryLock(
	ctx context.Context,
	pool *pgxpool.Pool,
	timeout time.Duration,
	fn func(*pgxpool.Conn) error,
) (returnErr error) {
	lock, err := acquireAdvisoryLock(ctx, pool, timeout)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, lock.Release(ctx))
	}()
	return fn(lock.conn)
}

// Release unlocks on the exact PostgreSQL session that acquired the lock.
// Cleanup is bounded independently of caller cancellation. If unlock fails,
// or the session is marked discard, the connection is discarded (closing
// it also releases the session lock).
func (l *advisoryLock) Release(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true

	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), bootstrapUnlockTimeout,
	)
	defer cancel()

	var unlocked bool
	err := l.conn.QueryRow(
		cleanupCtx,
		"SELECT pg_advisory_unlock(hashtext($1))",
		bootstrapAdvisoryLockKey,
	).Scan(&unlocked)
	if err == nil && unlocked && !l.discard {
		l.conn.Release()
		return nil
	}

	closeErr := discardPinnedConn(l.conn)
	if err != nil {
		return errors.Join(fmt.Errorf("release advisory lock: %w", err), closeErr)
	}
	if !unlocked {
		return errors.Join(
			errors.New("release advisory lock: lock not owned"), closeErr,
		)
	}
	return closeErr
}

func discardPinnedConn(conn *pgxpool.Conn) error {
	rawConn := conn.Hijack()
	closeCtx, cancel := context.WithTimeout(
		context.Background(), bootstrapUnlockTimeout,
	)
	defer cancel()
	return rawConn.Close(closeCtx)
}

func schemaExists(ctx context.Context, db bootstrapDB) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var one int
	err := db.QueryRow(
		qctx,
		"SELECT 1 FROM information_schema.schemata "+
			"WHERE schema_name = 'sage'",
	).Scan(&one)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func createFullSchema(ctx context.Context, db bootstrapDB) error {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx, err := db.Begin(qctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(qctx)

	_, err = tx.Exec(qctx, fullSchemaDDL)
	if err != nil {
		return fmt.Errorf(
			"executing schema DDL: %w\n"+
				"hint: if the user lacks CREATE privilege, "+
				"run as superuser: CREATE SCHEMA sage; "+
				"GRANT ALL ON SCHEMA sage TO sage_agent;", err)
	}

	if err := tx.Commit(qctx); err != nil {
		return fmt.Errorf("commit schema DDL: %w", err)
	}
	if err := runMigrations(ctx, db); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return nil
}

func ensureTablesExist(ctx context.Context, db bootstrapDB) error {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for _, tbl := range expectedTables {
		var one int
		err := db.QueryRow(
			qctx,
			"SELECT 1 FROM information_schema.tables "+
				"WHERE table_schema = 'sage' AND table_name = $1",
			tbl.name,
		).Scan(&one)
		if err != nil {
			// Table missing — create it.
			_, execErr := db.Exec(qctx, tbl.ddl)
			if execErr != nil {
				return fmt.Errorf("creating table sage.%s: %w", tbl.name, execErr)
			}
		}
	}

	// Run idempotent migrations for existing schemas.
	if err := runMigrations(ctx, db); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return nil
}
