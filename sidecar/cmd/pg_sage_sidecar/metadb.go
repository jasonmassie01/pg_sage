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

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
)

const (
	adminEmail    = "admin@pg-sage.local"
	adminPassLen  = 16
	metaDBTimeout = 10 * time.Second
)

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
	var encKey []byte
	if encKeyPassphrase != "" {
		salt, err := schema.ReadOrCreateKDFSalt(ctx, metaPool)
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

	// Tag every pool connection with a stable application_name so the
	// analyzer/executor can recognize all sidecar backends (not just the
	// one returned by pg_backend_pid() on some arbitrary pool conn) when
	// deciding whether a query is safe to terminate.
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["application_name"] = "pg_sage"

	const maxAttempts = 5
	backoff := 1 * time.Second
	var lastErr error

	for attempt := range maxAttempts {
		p, err := pgxpool.NewWithConfig(
			context.Background(), poolCfg)
		if err != nil {
			lastErr = fmt.Errorf("creating pool: %w", err)
			logRetry("connect", attempt, maxAttempts,
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
		lastErr = fmt.Errorf("cannot connect: %w", err)
		if attempt < maxAttempts-1 {
			logRetry("connect", attempt, maxAttempts,
				backoff, lastErr)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return nil, fmt.Errorf(
		"failed after %d attempts: %w", maxAttempts, lastErr)
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
	for _, rec := range records {
		registerStoreDatabase(state, rec)
	}

	go fleetReconnectLoop(state)
}

// registerStoreDatabase connects to a database from a store
// record and registers it with the fleet manager.
func registerStoreDatabase(
	state *metaDBState, rec store.DatabaseRecord,
) {
	connStr, err := state.Store.GetConnectionString(
		context.Background(), rec.ID,
	)
	if err != nil {
		logError("meta-db", "db %q: get connection: %v",
			rec.Name, err)
		registerFailedInstance(rec, err.Error())
		return
	}

	dbPool, err := connectMonitoredDB(connStr, rec.MaxConnections)
	if err != nil {
		logError("meta-db", "db %q: connect: %v", rec.Name, err)
		registerFailedInstance(rec, err.Error())
		return
	}

	bootstrapAndRegister(rec, dbPool)
}

// registerFleetRuntimeDatabase reconnects a fleet-mode database whose
// catalog row was edited in the UI. In YAML fleet mode the catalog row
// stores only a password placeholder; the live password comes from the
// existing runtime config and is passed in by the caller.
func registerFleetRuntimeDatabase(
	rec store.DatabaseRecord, password string,
) {
	dbCfg := storeRecordToDBConfig(rec)
	dbCfg.Password = password

	dbPool, err := connectMonitoredDB(
		dbCfg.ConnString(), rec.MaxConnections)
	if err != nil {
		logError("fleet", "db %q: reconnect after update: %v",
			rec.Name, err)
		registerFailedInstance(rec, err.Error())
		return
	}

	bootstrapAndRegister(rec, dbPool)
}

// registerFailedInstance adds a non-connected instance to the
// fleet for visibility in the dashboard.
func registerFailedInstance(rec store.DatabaseRecord, errMsg string) {
	dbCfg := storeRecordToDBConfig(rec)
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   rec.Name,
		Config: dbCfg,
		Status: &fleet.InstanceStatus{
			Error:    errMsg,
			LastSeen: time.Now(),
		},
	})
}

// bootstrapAndRegister runs schema bootstrap, creates components,
// and registers a healthy instance with the fleet manager.
func bootstrapAndRegister(
	rec store.DatabaseRecord, dbPool *pgxpool.Pool,
) {
	ctx := context.Background()

	if err := schema.Bootstrap(ctx, dbPool); err != nil {
		logWarn("meta-db", "db %q: schema bootstrap: %v",
			rec.Name, err)
	}
	schema.ReleaseAdvisoryLock(ctx, dbPool)

	dbPGVersion := detectPGVersion(dbPool)

	// Derive a per-instance context from the process shutdownCtx so
	// that RemoveInstance can cancel the collector, analyzer, and
	// orchestrator goroutines without also taking down the whole
	// process. Without this, deleting a fleet DB closes its pool but
	// leaves its goroutines spinning on shutdownCtx (they spam "pool
	// closed" errors until process exit). EmergencyStop intentionally
	// does not cancel this context; it only blocks action execution.
	instCtx, instCancel := context.WithCancel(shutdownCtx)

	dbColl := collector.New(
		dbPool, cfg, dbPGVersion, logStructuredWrapper,
	)
	go dbColl.Run(instCtx)

	// LLM features for meta-db registered databases.
	dbOpt, dbAdvIface, dbTuner, dbBrief :=
		buildFleetLLMFeatures(dbPool, dbPGVersion, dbColl,
			rec.DatabaseName)

	// dbTuner is a *tuner.Tuner which may be nil when cfg.Tuner.Enabled
	// is false. Passing the typed nil directly produces a non-nil
	// analyzer.QueryTuner interface that panics on Tune(). Normalize
	// to a true-nil interface before handing to analyzer.New.
	var qt analyzer.QueryTuner
	if dbTuner != nil {
		qt = dbTuner
	}
	dbAnal := analyzer.New(
		dbPool, cfg, dbColl, dbOpt, dbAdvIface, nil, qt,
		logStructuredWrapper,
	)
	go dbAnal.Run(instCtx)

	dbExec := buildExecutor(rec, dbPool, dbAnal)

	registerHealthyInstance(rec, dbPool, dbColl, dbAnal, dbExec, instCancel)

	// Populate findings immediately then start orchestrator.
	if inst := fleetMgr.GetInstance(rec.Name); inst != nil {
		updateInstanceFindings(ctx, inst)
	}
	dbCfg := storeRecordToDBConfig(rec)
	go fleetDBOrchestrator(
		instCtx, rec.Name, dbPool, dbExec, dbBrief, dbCfg)
}

// fleetReconnectLoop periodically checks for failed instances and
// attempts to reconnect with exponential backoff. Runs every 30s,
// caps backoff at 5 minutes per database.
func fleetReconnectLoop(state *metaDBState) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			retryFailedInstances(state)
		case <-shutdownCtx.Done():
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
		if inst.Stopped {
			continue // manually stopped
		}

		logInfo("reconnect", "attempting reconnect for %q", name)

		ctx, cancel := context.WithTimeout(
			context.Background(), metaDBTimeout,
		)
		records, err := loadDatabasesFromStore(ctx, state.Store)
		cancel()
		if err != nil {
			logWarn("reconnect", "load databases: %v", err)
			return
		}

		for _, rec := range records {
			if rec.Name != name {
				continue
			}
			connStr, err := state.Store.GetConnectionString(
				context.Background(), rec.ID)
			if err != nil {
				logWarn("reconnect",
					"db %q: get connection: %v", name, err)
				break
			}
			dbPool, err := connectMonitoredDB(
				connStr, rec.MaxConnections)
			if err != nil {
				logWarn("reconnect",
					"db %q: still unreachable: %v", name, err)
				break
			}
			// Success — bootstrap and re-register.
			logInfo("reconnect",
				"db %q: reconnected successfully", name)
			bootstrapAndRegister(rec, dbPool)
			break
		}
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

// buildExecutor creates an executor with action store for a
// store-managed database.
func buildExecutor(
	rec store.DatabaseRecord,
	dbPool *pgxpool.Pool, dbAnal *analyzer.Analyzer,
) *executor.Executor {
	ctx := context.Background()
	// Honour the YAML-configured trust.ramp_start on first bootstrap
	// of a store-managed database. Once sage.config has a row, the
	// stored value wins and configRampStart is ignored.
	rStart, _ := schema.PersistTrustRampStart(
		ctx, dbPool, configRampStart,
	)
	dbExec := executor.New(
		dbPool, cfg, dbAnal, rStart, logStructuredWrapper,
	)
	dbActionStore := store.NewActionStore(dbPool)
	dbExec.WithActionStore(dbActionStore, resolveExecMode(rec))
	go store.StartActionExpiry(
		shutdownCtx, dbActionStore, logStructuredWrapper,
	)
	return dbExec
}

// registerHealthyInstance registers a connected instance with the
// fleet manager. cancel stops the per-instance goroutines and is
// invoked by RemoveInstance.
func registerHealthyInstance(
	rec store.DatabaseRecord, dbPool *pgxpool.Pool,
	dbColl *collector.Collector, dbAnal *analyzer.Analyzer,
	dbExec *executor.Executor,
	cancel context.CancelFunc,
) {
	dbCfg := storeRecordToDBConfig(rec)
	inst := &fleet.DatabaseInstance{
		Name:      rec.Name,
		Config:    dbCfg,
		Pool:      dbPool,
		Collector: dbColl,
		Analyzer:  dbAnal,
		Executor:  dbExec,
		Cancel:    cancel,
		Status: &fleet.InstanceStatus{
			Connected:    true,
			TrustLevel:   rec.TrustLevel,
			DatabaseName: rec.Name,
			LastSeen:     time.Now(),
		},
	}
	fleetMgr.RegisterInstance(inst)
	logInfo("meta-db", "db %q: initialized", rec.Name)
}

// storeRecordToDBConfig converts a store.DatabaseRecord to a
// config.DatabaseConfig for the fleet manager.
func storeRecordToDBConfig(
	rec store.DatabaseRecord,
) config.DatabaseConfig {
	return config.DatabaseConfig{
		Name:           rec.Name,
		Host:           rec.Host,
		Port:           rec.Port,
		Database:       rec.DatabaseName,
		User:           rec.Username,
		SSLMode:        rec.SSLMode,
		MaxConnections: rec.MaxConnections,
		TrustLevel:     rec.TrustLevel,
	}
}

// resolveExecMode returns the execution mode for a store record.
func resolveExecMode(rec store.DatabaseRecord) string {
	if rec.ExecutionMode != "" {
		return rec.ExecutionMode
	}
	return "auto"
}
