package main

import (
	"context"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
)

// initStandalone prepares the monitored database, which in standalone mode
// is also the control database, then builds its runtime.
func initStandalone() {
	ctx := context.Background()
	name := resolveDBName()
	logInfo("startup", "running prerequisite checks and schema bootstrap…")
	checks, err := prepareMonitoredDatabase(ctx, pool, name, true)
	if err != nil {
		logError("startup", "%v", err)
		os.Exit(1)
	}
	if err := initializeConfigController(pool); err != nil {
		logError("startup", "config controller: %v", err)
		os.Exit(1)
	}
	if err := bootstrapAdminIfEmpty(ctx, pool); err != nil {
		logWarn("startup", "admin bootstrap: %v", err)
	}
	initializeAnalyzeSemaphore()
	executor.VerifyGrants(ctx, pool, cfg.Postgres.User, logStructuredWrapper)
	if cfg.Trust.Level == "autonomous" && cfg.Trust.Tier3Moderate &&
		cfg.Trust.MaintenanceWindow == "" {
		logWarn("startup", "tier3_moderate enabled without maintenance_window — "+
			"moderate actions will NOT execute")
	}
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	registerLLMConfigOwner()
	llmMgr = newStandaloneLLMManager(llmClient)
	rt, err := buildDatabaseRuntime(ctx, databaseRuntimeSpec{
		Scope: "startup", Name: name, Config: buildDBConfig(name),
		Pool: pool, ControlPool: pool, ExecMode: resolveExecutionMode(),
		Parent: shutdownCtx, RequireChecks: true, Checks: checks, Shared: true,
	})
	if err != nil {
		logError("startup", "database runtime: %v", err)
		os.Exit(1)
	}
	coll, anal, exec, actionStore = rt.collector, rt.analyzer, rt.executor, rt.actions
	fleetMgr = fleet.NewManager(cfg)
	rt.publish(fleetMgr)
	logInfo("startup", "standalone mode initialized — collector=%ds, analyzer=%ds, trust=%s",
		cfg.Collector.IntervalSeconds, cfg.Analyzer.IntervalSeconds, cfg.Trust.Level)
}

// initFleetMultiDB builds one runtime per YAML-configured database. The
// first database that connects is the control database: it holds the
// admin user, the standing policy and the notification rules.
func initFleetMultiDB() {
	fleetMgr = fleet.NewManager(cfg)
	initializeAnalyzeSemaphore()

	// LLM client + manager (shared across fleet).
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	registerLLMConfigOwner()
	llmMgr = llm.NewManager(llmClient, nil, false)

	// Per-database LLM token budget (F5): split a fleet-wide daily cap
	// across databases so one noisy DB can't drain the whole budget.
	names := make([]string, 0, len(cfg.Databases))
	for _, database := range cfg.Databases {
		names = append(names, database.Name)
	}
	initializeFleetBudget(names)

	boot := &fleetBootstrap{}
	for _, dbCfg := range cfg.Databases {
		boot.start(dbCfg)
	}
	registerFleetDatabasesOwner(boot)
	// Register fleet databases in sage.databases for config API.
	if boot.controlPool != nil {
		registerFleetDatabases(boot.controlPool)
	}
	logInfo("fleet", "%d of %d configured databases initialized",
		boot.initialized, len(cfg.Databases))
}

// registerFleetDatabases upserts all YAML-defined fleet databases
// into sage.databases on the config pool so the per-database config
// API can reference them by ID.
func registerFleetDatabases(configPool *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second)
	defer cancel()

	for _, dbCfg := range cfg.Databases {
		trustLevel := dbCfg.TrustLevel
		if trustLevel == "" {
			trustLevel = cfg.Trust.Level
		}
		dbID, err := upsertFleetDatabase(
			ctx, configPool, dbCfg, trustLevel)
		if err != nil {
			logWarn("fleet",
				"db %q: register in sage.databases: %v",
				dbCfg.Name, err)
			continue
		}
		if inst := fleetMgr.GetInstance(dbCfg.Name); inst != nil {
			inst.DatabaseID = dbID
		}
		logInfo("fleet",
			"db %q: registered as database ID %d",
			dbCfg.Name, dbID)
	}
}

// upsertFleetDatabase inserts or updates a row in sage.databases
// for a YAML-configured fleet database. Returns the database ID.
func upsertFleetDatabase(
	ctx context.Context, pool *pgxpool.Pool,
	dbCfg config.DatabaseConfig, trustLevel string,
) (int, error) {
	var id int
	err := pool.QueryRow(ctx, `
		INSERT INTO sage.databases
			(name, host, port, database_name, username,
			 password_enc, sslmode, max_connections,
			 trust_level, execution_mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'auto')
		ON CONFLICT (name) DO UPDATE SET
			host = EXCLUDED.host,
			port = EXCLUDED.port,
			database_name = EXCLUDED.database_name,
			username = EXCLUDED.username,
			sslmode = EXCLUDED.sslmode,
			max_connections = EXCLUDED.max_connections,
			trust_level = EXCLUDED.trust_level,
			updated_at = now()
		RETURNING id`,
		dbCfg.Name, dbCfg.Host, dbCfg.Port,
		dbCfg.Database, dbCfg.User,
		[]byte{0}, // placeholder — fleet uses YAML creds
		dbCfg.SSLMode, dbCfg.MaxConnections,
		trustLevel,
	).Scan(&id)
	return id, err
}

func resolveDBName() string {
	if len(cfg.Databases) > 0 && cfg.Databases[0].Name != "" {
		return cfg.Databases[0].Name
	}
	if cfg.Postgres.Database != "" {
		return cfg.Postgres.Database
	}
	return "default"
}

func buildDBConfig(name string) config.DatabaseConfig {
	if len(cfg.Databases) > 0 {
		return cfg.Databases[0]
	}
	return config.DatabaseConfig{
		Name:     name,
		Host:     cfg.Postgres.Host,
		Port:     cfg.Postgres.Port,
		User:     cfg.Postgres.User,
		Database: cfg.Postgres.Database,
		SSLMode:  cfg.Postgres.SSLMode,
	}
}

// resolveExecutionMode returns the execution mode from config.
// Standalone mode defaults to "auto"; fleet databases have their
// own execution_mode per database record.
func resolveExecutionMode() string {
	if len(cfg.Databases) > 0 {
		// Use first database's config if available.
		return resolveStaticFleetExecMode(cfg.Databases[0])
	}
	return "auto"
}

func resolveStaticFleetExecMode(dbCfg config.DatabaseConfig) string {
	mode := dbCfg.ExecutionMode
	if mode == "" {
		mode = cfg.Defaults.ExecutionMode
	}
	switch mode {
	case "auto", "approval", "manual":
		return mode
	default:
		return "auto"
	}
}
