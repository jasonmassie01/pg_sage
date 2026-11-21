package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/store"
)

const metaDBTimeout = 10 * time.Second

var bootstrapManagedDatabaseSchema = schema.Bootstrap

// metaSchemaBootstrap is the meta database's schema boot (a test seam).
var metaSchemaBootstrap = schema.Bootstrap

// metaDBState holds state derived from the --meta-db flag.
type metaDBState struct {
	Pool       *pgxpool.Pool
	EncryptKey []byte
	// Keys holds EncryptKey's keyring and the previous key of a rotation.
	Keys  *controlKeySet
	Store *store.DatabaseStore
}

// connectMetaDB creates a connection pool for the metadata database
// with exponential backoff. Retries up to 5 times with delays of
// 1s, 2s, 4s, 8s before giving up; each attempt resolves the Lakebase
// endpoint afresh.
func connectMetaDB(dsn string) (*pgxpool.Pool, error) {
	base, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing meta-db DSN: %w", err)
	}
	base.MaxConns = 5
	p, err := retryStartup(context.Background(), "meta-db", startupAttempts,
		func(ctx context.Context) (*pgxpool.Pool, error) {
			return openStartupPool(ctx, base, func(poolCfg *pgxpool.Config) {
				poolCfg.MinConns = 1
				poolCfg.MaxConnLifetime = 30 * time.Minute
				poolCfg.MaxConnIdleTime = 5 * time.Minute
				// pg_sage's sessions are named and its statements tagged,
				// not hidden from pg_stat_statements: a DBA sees its cost
				// (perf v1.8.3).
				selfmonitor.ConfigurePool(poolCfg)
			})
		})
	warnLakebaseScaleToZero("meta-db", p, 0) // no collector reads the meta-db
	return p, err
}

// bootstrapMetaSchema runs the schema boot within its own metaDBTimeout, so
// a slow boot cannot spend the budget of the steps after it (the admin's
// password hash among them).
func bootstrapMetaSchema(metaPool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(context.Background(), metaDBTimeout)
	defer cancel()
	if err := metaSchemaBootstrap(ctx, metaPool); err != nil {
		return fmt.Errorf("bootstrapping meta-db schema: %w", err)
	}
	return nil
}

// initMetaDB bootstraps the meta database: schema, admin user,
// and returns state needed by the rest of startup. The schema boot and
// the steps after it each get metaDBTimeout.
func initMetaDB(
	metaPool *pgxpool.Pool, encKeyPassphrase string,
) (*metaDBState, error) {
	if err := bootstrapMetaSchema(metaPool); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), metaDBTimeout,
	)
	defer cancel()
	warnIsolationDefault(ctx, metaPool, "meta-db", logWarn)

	// Run config schema migration (adds database_id, audit table).
	if err := schema.MigrateConfigSchema(ctx, metaPool); err != nil {
		return nil, fmt.Errorf("config schema migration: %w", err)
	}

	// The keys are derived with the per-deployment KDF salt persisted in
	// sage.crypto_meta; without a passphrase a generated key may be settled
	// here (security-08). Losing the salt or the key loses the secrets.
	keys, err := controlKeysFor(ctx, metaPool, encKeyPassphrase)
	if err != nil {
		return nil, fmt.Errorf("meta-db encryption key: %w", err)
	}
	var encKey []byte
	if keys != nil {
		encKey = keys.active
	} else {
		log.Println("WARNING: no encryption key; database credentials cannot be stored")
	}

	// Bootstrap admin user if none exist.
	if err := bootstrapAdminUser(ctx, metaPool); err != nil {
		return nil, fmt.Errorf("admin bootstrap: %w", err)
	}

	dbStore, err := metaDatabaseStore(ctx, metaPool, keys, encKeyPassphrase)
	if err != nil {
		return nil, err
	}

	return &metaDBState{
		Pool:       metaPool,
		EncryptKey: encKey,
		Keys:       keys,
		Store:      dbStore,
	}, nil
}

// loadDatabasesFromStore reads the databases that get a runtime: enabled
// full members (an observe-only member is swept, fleetpol-01d).
func loadDatabasesFromStore(
	ctx context.Context, dbStore *store.DatabaseStore,
) ([]store.DatabaseRecord, error) {
	records, err := dbStore.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing databases from store: %w", err)
	}

	var enabled []store.DatabaseRecord
	for _, r := range records {
		if r.RunsRuntime() {
			enabled = append(enabled, r)
		}
	}
	return enabled, nil
}

// connectMonitoredDB creates and pings a pool for a monitored DB
// with exponential backoff. Retries up to 5 times with delays of
// 1s, 2s, 4s, 8s before giving up.
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
	base, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid DSN: %w", err)
	}
	base.MaxConns = int32(maxConns)
	p, err := retryStartup(ctx, "connect", startupAttempts,
		func(ctx context.Context) (*pgxpool.Pool, error) {
			return openStartupPool(ctx, base, tuneMonitoredPool)
		})
	warnLakebaseScaleToZero("connect", p, monitoredCollectorInterval())
	return p, err
}

func tuneMonitoredPool(poolCfg *pgxpool.Config) {
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
}

// initMetaDBFleet loads databases from the meta-db store and
// initializes fleet instances for each enabled database.
// Starts a background goroutine to reconnect failed databases.
func initMetaDBFleet(state *metaDBState) {
	fleetMgr = fleet.NewManager(cfg)
	// fleet-09: the fleet-wide stop lives in the meta database; restore it
	// before any database registers.
	fleetMgr.UseFleetStopStore(fleet.NewPGFleetStopStore(state.Pool))
	initializeAnalyzeSemaphore()
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	registerLLMConfigOwner()
	startBriefingRollup()
	learnLLMInputLimits()
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

	logInfo("meta-db", "found %d enabled databases; fleet-learning boundaries: %s",
		len(records), metaBoundarySummary(records))
	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Name)
		llmUsageIDs.setID(record.Name, record.ID)
	}
	initializeFleetBudget(names)
	startLLMUsageLedger(state.Pool) // fleet-10: before any database registers
	warnMissingSLOOwners(cfg.SRE.SLO, names, logStructuredWrapper)
	startHistoryStoreCleaner(state.Pool) // history_store_wiring.go
	for _, rec := range records {
		registerStoreDatabase(state, rec)
	}

	startFleetDiscovery(state) // fleet_discovery_wiring.go (market-06)
	startObserveSweeper(state) // observe_only_wiring.go (fleetpol-01d)
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
		logStorePrepareFailure(rec.Name, err)
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
	expected := inst.ConfigSnapshot().Database
	if expected != "" && databaseName != expected {
		return fmt.Errorf(
			"health check connected to %q, expected %q",
			databaseName, expected,
		)
	}
	return nil
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
