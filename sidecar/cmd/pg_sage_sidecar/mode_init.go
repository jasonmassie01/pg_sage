package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	executor.VerifyGrants(ctx, pool, cfg.Postgres.User, cfg.Trust.Level,
		logStructuredWrapper)
	if cfg.Trust.Level == "autonomous" && cfg.Trust.Tier3Moderate &&
		cfg.Trust.MaintenanceWindow == "" {
		logWarn("startup", "tier3_moderate enabled without maintenance_window — "+
			"moderate actions will NOT execute")
	}
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	registerLLMConfigOwner()
	learnLLMInputLimits()
	llmMgr = newStandaloneLLMManager(llmClient)
	startLLMUsageLedger(pool) // fleet-10: before the runtime's first call
	dbCfg := buildDBConfig(name)
	actionPool, err := openActionPool(cfg.Postgres.DSN(), dbCfg)
	if err != nil {
		logError("startup", "%v", err)
		os.Exit(1)
	}
	fleetMgr = fleet.NewManager(cfg) // env-race: before any runtime worker reads it
	rt, err := buildDatabaseRuntime(ctx, databaseRuntimeSpec{
		Scope: "startup", Name: name, Config: dbCfg, Pool: pool, ActionPool: actionPool,
		ControlPool: pool, ExecMode: resolveExecutionMode(),
		Parent: shutdownCtx, RequireChecks: true, Checks: checks, Shared: true,
	})
	if err != nil {
		logError("startup", "database runtime: %v", err)
		os.Exit(1)
	}
	coll, anal, exec, actionStore = rt.collector, rt.analyzer, rt.executor, rt.actions
	rt.publish(fleetMgr)
	logInfo("startup", "standalone mode initialized — collector=%ds, analyzer=%ds, trust=%s",
		cfg.Collector.IntervalSeconds, cfg.Analyzer.IntervalSeconds, cfg.Trust.Level)
}

// initFleetMultiDB builds one runtime per YAML-configured database. The
// first database that connects is the control database: it holds the
// admin user, the standing policy and the notification rules. A pinned
// fleet_learning.control_database is tried first.
func initFleetMultiDB() {
	fleetMgr = fleet.NewManager(cfg)
	initializeAnalyzeSemaphore()

	// LLM client + manager (shared across fleet).
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	registerLLMConfigOwner()
	startBriefingRollup()
	learnLLMInputLimits()
	llmMgr = llm.NewManager(llmClient, nil, false)

	// Per-database LLM token budget (F5): split a fleet-wide daily cap
	// across databases so one noisy DB can't drain the whole budget.
	names := make([]string, 0, len(cfg.Databases))
	for _, database := range cfg.Databases {
		names = append(names, database.Name)
	}
	initializeFleetBudget(names)

	boot := &fleetBootstrap{}
	for _, dbCfg := range fleetStartOrder(cfg.Databases, cfg.FleetLearning.ControlDatabase) {
		boot.start(dbCfg)
	}
	registerFleetDatabasesOwner(boot)
	// fleet-10: the control database is known only once one connected.
	startLLMUsageLedger(boot.controlPool)
	startPolicyExceptionSweep(shutdownCtx, boot.controlPool) // fleetpol-01a
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
	tags, err := json.Marshal(fleetTagMap(dbCfg.Tags))
	if err != nil {
		return 0, fmt.Errorf("encode tags of %q: %w", dbCfg.Name, err)
	}
	// Tags reach sage.databases so policy-set selectors see them.
	err = pool.QueryRow(ctx, `
		INSERT INTO sage.databases
			(name, host, port, database_name, username,
			 password_enc, sslmode, max_connections,
			 trust_level, execution_mode, tags)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'auto', $10)
		ON CONFLICT (name) DO UPDATE SET
			host = EXCLUDED.host,
			port = EXCLUDED.port,
			database_name = EXCLUDED.database_name,
			username = EXCLUDED.username,
			sslmode = EXCLUDED.sslmode,
			max_connections = EXCLUDED.max_connections,
			trust_level = EXCLUDED.trust_level,
			tags = EXCLUDED.tags,
			updated_at = now()
		RETURNING id`,
		dbCfg.Name, dbCfg.Host, dbCfg.Port,
		dbCfg.Database, dbCfg.User,
		[]byte{0}, // placeholder — fleet uses YAML creds
		dbCfg.SSLMode, dbCfg.MaxConnections,
		trustLevel, tags,
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
		Provider: cfg.Postgres.Provider, CloudResource: cfg.Postgres.CloudResource,
		ActionDSN: cfg.Postgres.ActionDSN,
		CNPG:      cfg.Postgres.CNPG,
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
