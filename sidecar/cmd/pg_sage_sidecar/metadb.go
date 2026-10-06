package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/store"
)

const (
	adminEmail    = "admin@pg-sage.local"
	adminPassLen  = 16
	metaDBTimeout = 10 * time.Second
)

var bootstrapManagedDatabaseSchema = schema.Bootstrap

// metaDBState holds state derived from the --meta-db flag.
type metaDBState struct {
	Pool       *pgxpool.Pool
	EncryptKey []byte
	Store      *store.DatabaseStore
}

// connectMetaDB creates a connection pool for the metadata database
// with exponential backoff. Retries up to 5 times with delays of
// 1s, 2s, 4s, 8s, 16s before giving up.
func connectMetaDB(dsn string) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing meta-db DSN: %w", err)
	}
	poolCfg.MaxConns = 5
	poolCfg.MinConns = 1
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	// pg_sage's sessions are named and its statements tagged, not hidden
	// from pg_stat_statements: a DBA sees its cost (perf v1.8.3).
	selfmonitor.ConfigurePool(poolCfg)

	const maxAttempts = 5
	backoff := 1 * time.Second
	var lastErr error

	for attempt := range maxAttempts {
		p, err := pgxpool.NewWithConfig(
			context.Background(), poolCfg)
		if err != nil {
			lastErr = fmt.Errorf("creating meta-db pool: %w", err)
			logRetry("meta-db", attempt, maxAttempts,
				backoff, lastErr)
			time.Sleep(backoff)
			backoff *= 2
			continue
		}

		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		err = p.Ping(ctx)
		cancel()
		if err == nil {
			return p, nil
		}
		p.Close()
		lastErr = fmt.Errorf("pinging meta-db: %w", err)
		if attempt < maxAttempts-1 {
			logRetry("meta-db", attempt, maxAttempts,
				backoff, lastErr)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return nil, fmt.Errorf(
		"meta-db failed after %d attempts: %w",
		maxAttempts, lastErr)
}

// initMetaDB bootstraps the meta database: schema, admin user,
// and returns state needed by the rest of startup.
func initMetaDB(
	metaPool *pgxpool.Pool, encKeyPassphrase string,
) (*metaDBState, error) {
	ctx, cancel := context.WithTimeout(
		context.Background(), metaDBTimeout,
	)
	defer cancel()

	// Bootstrap sage.* schema on the meta database.
	if err := schema.Bootstrap(ctx, metaPool); err != nil {
		return nil, fmt.Errorf("bootstrapping meta-db schema: %w", err)
	}

	// Run config schema migration (adds database_id, audit table).
	if err := schema.MigrateConfigSchema(ctx, metaPool); err != nil {
		return nil, fmt.Errorf("config schema migration: %w", err)
	}

	// Derive encryption key if provided. A per-deployment random
	// salt is persisted in sage.crypto_meta on first bootstrap and
	// reused on every subsequent startup, so keys derived here
	// match keys used to encrypt prior records. The salt must be
	// stable across restarts — if it is lost, all encrypted
	// credentials become unrecoverable.
	var encKey, salt []byte
	if encKeyPassphrase != "" {
		var err error
		salt, err = schema.ReadOrCreateKDFSalt(ctx, metaPool)
		if err != nil {
			return nil, fmt.Errorf("kdf salt: %w", err)
		}
		encKey = crypto.DeriveKey(encKeyPassphrase, salt)
	} else {
		log.Println(
			"WARNING: --encryption-key not set; " +
				"database credentials cannot be encrypted",
		)
	}

	// Bootstrap admin user if none exist.
	if err := bootstrapAdminUser(ctx, metaPool); err != nil {
		return nil, fmt.Errorf("admin bootstrap: %w", err)
	}

	dbStore := store.NewDatabaseStore(metaPool, encKey)
	if encKeyPassphrase != "" {
		// Credentials written by v0.8.4/v0.8.5 used legacy key
		// derivations; decrypt and re-encrypt them on first read (G5-B05).
		dbStore.WithKeyMigration(encKeyPassphrase, salt)
	}

	return &metaDBState{
		Pool:       metaPool,
		EncryptKey: encKey,
		Store:      dbStore,
	}, nil
}

// bootstrapAdminUser creates the first admin user when the
// sage.users table is empty. Prints credentials to stdout.
func bootstrapAdminUser(
	ctx context.Context, pool *pgxpool.Pool,
) error {
	count, err := auth.UserCount(ctx, pool)
	if err != nil {
		return fmt.Errorf("checking user count: %w", err)
	}
	if count > 0 {
		return nil
	}

	password, err := generateRandomPassword(adminPassLen)
	if err != nil {
		return fmt.Errorf("generating admin password: %w", err)
	}

	if err := auth.BootstrapAdmin(
		ctx, pool, adminEmail, password,
	); err != nil {
		return fmt.Errorf("creating admin: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"First admin created: %s\nInitial password: %s\nChange this password immediately.\n",
		adminEmail, password,
	)
	return nil
}

// generateRandomPassword returns a hex-encoded random string of
// the given length.
func generateRandomPassword(length int) (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(buf)[:length], nil
}

// loadDatabasesFromStore reads enabled databases from the store.
func loadDatabasesFromStore(
	ctx context.Context, dbStore *store.DatabaseStore,
) ([]store.DatabaseRecord, error) {
	records, err := dbStore.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing databases from store: %w", err)
	}

	var enabled []store.DatabaseRecord
	for _, r := range records {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	return enabled, nil
}

// connectMonitoredDB creates and pings a pool for a monitored DB
// with exponential backoff. Retries up to 5 times with delays of
// 1s, 2s, 4s, 8s, 16s before giving up.
func connectMonitoredDB(
	dsn string, maxConns int,
) (*pgxpool.Pool, error) {
	return connectMonitoredDBContext(
		context.Background(), dsn, maxConns,
	)
}

func connectMonitoredDBContext(
	ctx context.Context, dsn string, maxConns int,
) (*pgxpool.Pool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid DSN: %w", err)
	}
	poolCfg.MaxConns = int32(maxConns)
	if poolCfg.MaxConns < 2 {
		poolCfg.MaxConns = 2
	}
	poolCfg.MinConns = 1
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 30 * time.Second

	// A stable application_name lets the analyzer and executor recognize
	// every sidecar backend (not just pg_backend_pid() of one pool conn)
	// when deciding whether a query is safe to terminate; every statement
	// carries the /* pg_sage */ tag so pg_stat_statements shows pg_sage's
	// cost and pg_sage's own analysis leaves it out.
	selfmonitor.ConfigurePool(poolCfg)

	const maxAttempts = 5
	backoff := 1 * time.Second
	var lastErr error

	for attempt := range maxAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := pgxpool.NewWithConfig(ctx, poolCfg)
		if err != nil {
			lastErr = fmt.Errorf("creating pool: %w", err)
			if attempt < maxAttempts-1 {
				logRetry("connect", attempt, maxAttempts,
					backoff, lastErr)
				if err := waitForRetry(ctx, backoff); err != nil {
					return nil, err
				}
				backoff *= 2
			}
			continue
		}

		pingCtx, cancel := context.WithTimeout(
			ctx, 5*time.Second,
		)
		err = p.Ping(pingCtx)
		cancel()
		if err == nil {
			return p, nil
		}
		p.Close()
		lastErr = fmt.Errorf("cannot connect: %w", err)
		if attempt < maxAttempts-1 {
			logRetry("connect", attempt, maxAttempts,
				backoff, lastErr)
			if err := waitForRetry(ctx, backoff); err != nil {
				return nil, err
			}
			backoff *= 2
		}
	}
	return nil, fmt.Errorf(
		"failed after %d attempts: %w", maxAttempts, lastErr)
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func logRetry(
	component string, attempt, max int,
	backoff time.Duration, err error,
) {
	logWarn(component,
		"attempt %d/%d failed: %v — retrying in %s",
		attempt+1, max, err, backoff)
}

// initMetaDBFleet loads databases from the meta-db store and
// initializes fleet instances for each enabled database.
// Starts a background goroutine to reconnect failed databases.
func initMetaDBFleet(state *metaDBState) {
	fleetMgr = fleet.NewManager(cfg)
	initializeAnalyzeSemaphore()
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	registerLLMConfigOwner()
	llmMgr = llm.NewManager(llmClient, nil, false)

	ctx, cancel := context.WithTimeout(
		context.Background(), metaDBTimeout,
	)
	defer cancel()

	records, err := loadDatabasesFromStore(ctx, state.Store)
	if err != nil {
		logWarn("meta-db", "loading databases: %v", err)
		return
	}

	logInfo("meta-db", "found %d enabled databases", len(records))
	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Name)
	}
	initializeFleetBudget(names)
	startHistoryStoreCleaner(state.Pool) // history_store_wiring.go
	for _, rec := range records {
		registerStoreDatabase(state, rec)
	}

	go fleetReconnectLoop(shutdownCtx, state)
}

// registerStoreDatabase connects to a database from a store
// record and registers it with the fleet manager.
func registerStoreDatabase(
	state *metaDBState, rec store.DatabaseRecord,
) {
	inst, err := prepareStoreDatabase(
		context.Background(), state, rec,
	)
	if err != nil {
		logError("meta-db", "db %q: prepare: %v", rec.Name, err)
		registerFailedInstance(rec, err.Error())
		return
	}
	activateStoreDatabase(inst)
}

// registerFailedInstance adds a non-connected instance to the
// fleet for visibility in the dashboard.
func registerFailedInstance(rec store.DatabaseRecord, errMsg string) {
	fleetMgr.RegisterInstance(failedStoreInstance(rec, errMsg))
}

func prepareStoreDatabase(
	ctx context.Context, state *metaDBState, rec store.DatabaseRecord,
) (*fleet.DatabaseInstance, error) {
	connStr, err := state.Store.GetConnectionString(ctx, rec.ID)
	if err != nil {
		return nil, fmt.Errorf("get connection string: %w", err)
	}
	return prepareStoreDatabaseConnection(ctx, rec, connStr)
}

func prepareStoreDatabaseConnection(
	ctx context.Context, rec store.DatabaseRecord, connStr string,
) (*fleet.DatabaseInstance, error) {
	dbPool, err := connectMonitoredDBContext(ctx, connStr, rec.MaxConnections)
	if err != nil {
		return nil, err
	}
	inst, err := buildStoreDatabaseRuntime(ctx, rec, dbPool)
	if err != nil {
		dbPool.Close()
		return nil, err
	}
	return inst, nil
}

// buildStoreDatabaseRuntime builds a store-managed database's runtime. Its
// standing policy is scoped to the record and, with notifications, lives
// in the meta DB the API writes to (G5-B10, G5-B11).
func buildStoreDatabaseRuntime(
	ctx context.Context, rec store.DatabaseRecord, dbPool *pgxpool.Pool,
) (*fleet.DatabaseInstance, error) {
	rt, err := buildDatabaseRuntime(ctx, databaseRuntimeSpec{
		Scope: "meta-db", Name: rec.Name, DatabaseID: rec.ID,
		Config: storeRecordToDBConfig(rec), Pool: dbPool,
		ControlPool: notificationControlPool(globalMetaState, nil),
		ExecMode:    resolveExecMode(rec), Parent: shutdownCtx, RequireChecks: true,
	})
	if err != nil {
		return nil, err
	}
	return rt.inst, nil
}
func activateStoreDatabase(inst *fleet.DatabaseInstance) {
	activateStoreDatabaseWithManager(fleetMgr, inst)
}

func activateStoreDatabaseWithManager(
	mgr *fleet.DatabaseManager, inst *fleet.DatabaseInstance,
) {
	mgr.RegisterInstance(inst)
	updateInstanceFindings(context.Background(), inst)
	logInfo("meta-db", "db %q: initialized", inst.Name)
}

func healthCheckStoreDatabase(
	ctx context.Context, inst *fleet.DatabaseInstance,
) error {
	if inst == nil || inst.Pool == nil {
		return fleet.ErrInvalidInstance
	}
	if ctx == nil {
		ctx = context.Background()
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var databaseName string
	if err := inst.Pool.QueryRow(
		checkCtx, "SELECT current_database()",
	).Scan(&databaseName); err != nil {
		return fmt.Errorf("health check database: %w", err)
	}
	if inst.Config.Database != "" && databaseName != inst.Config.Database {
		return fmt.Errorf(
			"health check connected to %q, expected %q",
			databaseName, inst.Config.Database,
		)
	}
	return nil
}

// fleetReconnectLoop periodically checks for failed instances and
// attempts to reconnect with exponential backoff. Runs every 30s,
// caps backoff at 5 minutes per database.
func fleetReconnectLoop(ctx context.Context, state *metaDBState) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Rows changed outside this process's API (another replica,
			// SQL) converge first; failed databases then retry.
			runMetaReconcilePass(ctx, state)
			retryFailedInstances(state)
		case <-ctx.Done():
			return
		}
	}
}

// retryFailedInstances finds instances with errors or nil pools
// and attempts reconnection.
func retryFailedInstances(state *metaDBState) {
	instances := fleetMgr.Instances()
	for name, inst := range instances {
		snap := inst.SnapshotStatus()
		if inst.Pool != nil && snap.Error == "" {
			continue // healthy
		}
		if fleetMgr.InstanceStopped(inst) {
			continue // manually stopped
		}

		logInfo("reconnect", "attempting reconnect for %q", name)

		ctx, cancel := context.WithTimeout(
			context.Background(), metaDBTimeout,
		)
		records, err := loadDatabasesFromStore(ctx, state.Store)
		if err != nil {
			cancel()
			logWarn("reconnect", "load databases: %v", err)
			return
		}

		for _, rec := range records {
			if rec.Name != name {
				continue
			}
			candidate, err := prepareStoreDatabase(ctx, state, rec)
			if err != nil {
				logWarn("reconnect",
					"db %q: still unreachable: %v", name, err)
				break
			}
			// Success — bootstrap and re-register.
			err = fleetMgr.ReplaceInstanceIfCurrent(
				ctx, name, inst, candidate, healthCheckStoreDatabase,
			)
			if err != nil {
				if fleetMgr.GetInstance(candidate.Name) == candidate {
					logWarn("reconnect",
						"db %q: published but old runtime drain failed: %v",
						name, err)
				} else {
					logWarn("reconnect", "db %q: candidate rejected: %v",
						name, err)
				}
				break
			}
			updateInstanceFindings(context.Background(), candidate)
			logInfo("reconnect", "db %q: reconnected successfully", name)
			break
		}
		cancel()
	}
}

// detectPGVersion queries the PG version number from the pool and
// falls back to PG 14 if the query fails or the response is
// unparsable. Failures are logged so operators can spot config
// rules that silently use the default (e.g. tuner rules gated on
// a specific PG version).
func detectPGVersion(p *pgxpool.Pool) int {
	const fallback = 140000
	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second)
	defer cancel()

	var verStr string
	var ver int
	if err := p.QueryRow(
		ctx, "SHOW server_version_num",
	).Scan(&verStr); err != nil {
		logWarn("meta-db",
			"detectPGVersion: SHOW server_version_num failed: %v; "+
				"assuming PG %d", err, fallback/10000)
		return fallback
	}
	if _, err := fmt.Sscanf(verStr, "%d", &ver); err != nil || ver == 0 {
		logWarn("meta-db",
			"detectPGVersion: unparsable server_version_num %q: %v; "+
				"assuming PG %d", verStr, err, fallback/10000)
		return fallback
	}
	return ver
}

// storeRecordToDBConfig converts a store.DatabaseRecord to a
// config.DatabaseConfig for the fleet manager.
func storeRecordToDBConfig(
	rec store.DatabaseRecord,
) config.DatabaseConfig {
	return config.DatabaseConfig{
		Name:               rec.Name,
		Host:               rec.Host,
		Port:               rec.Port,
		Database:           rec.DatabaseName,
		User:               rec.Username,
		SSLMode:            rec.SSLMode,
		MaxConnections:     rec.MaxConnections,
		TrustLevel:         rec.TrustLevel,
		TrustLevelExplicit: true,
		ExecutionMode:      resolveExecMode(rec),
	}
}

// resolveExecMode returns the execution mode for a store record.
func resolveExecMode(rec store.DatabaseRecord) string {
	if rec.ExecutionMode != "" {
		return rec.ExecutionMode
	}
	return "auto"
}
