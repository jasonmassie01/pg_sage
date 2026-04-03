package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
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

	// Derive encryption key if provided.
	var encKey []byte
	if encKeyPassphrase != "" {
		encKey = crypto.DeriveKey(encKeyPassphrase)
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

	fmt.Printf(
		"First admin created: %s / %s\n", adminEmail, password,
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

	dbColl := collector.New(
		dbPool, cfg, dbPGVersion, logStructuredWrapper,
	)
	go dbColl.Run(shutdownCtx)

	// LLM features for meta-db registered databases.
	dbOpt, dbAdvIface, dbTuner, dbBrief :=
		buildFleetLLMFeatures(dbPool, dbPGVersion, dbColl,
			rec.DatabaseName)

	dbAnal := analyzer.New(
		dbPool, cfg, dbColl, dbOpt, dbAdvIface, nil, dbTuner,
		logStructuredWrapper,
	)
	go dbAnal.Run(shutdownCtx)

	dbExec := buildExecutor(rec, dbPool, dbAnal)

	registerHealthyInstance(rec, dbPool, dbColl, dbAnal, dbExec)

	// Populate findings immediately then start orchestrator.
	if inst := fleetMgr.GetInstance(rec.Name); inst != nil {
		updateInstanceFindings(ctx, inst)
	}
	dbCfg := storeRecordToDBConfig(rec)
	go fleetDBOrchestrator(
		rec.Name, dbPool, dbExec, dbBrief, dbCfg)
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
		if inst.Pool != nil && inst.Status.Error == "" {
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

// detectPGVersion queries the PG version number from the pool.
func detectPGVersion(p *pgxpool.Pool) int {
	var verStr string
	var ver int
	_ = p.QueryRow(
		context.Background(), "SHOW server_version_num",
	).Scan(&verStr)
	fmt.Sscanf(verStr, "%d", &ver)
	if ver == 0 {
		ver = 140000
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
	rStart, _ := schema.PersistTrustRampStart(
		ctx, dbPool, time.Time{},
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
// fleet manager.
func registerHealthyInstance(
	rec store.DatabaseRecord, dbPool *pgxpool.Pool,
	dbColl *collector.Collector, dbAnal *analyzer.Analyzer,
	dbExec *executor.Executor,
) {
	dbCfg := storeRecordToDBConfig(rec)
	inst := &fleet.DatabaseInstance{
		Name:      rec.Name,
		Config:    dbCfg,
		Pool:      dbPool,
		Collector: dbColl,
		Analyzer:  dbAnal,
		Executor:  dbExec,
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
