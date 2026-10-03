package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/store"
)

// Set by goreleaser ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Global state.
var (
	// analyzeSem serializes ANALYZE actions fleet-wide across
	// every Executor instance in this process. Sized from
	// cfg.Tuner.MaxConcurrentAnalyze at startup.
	analyzeSem       chan struct{}
	pool             *pgxpool.Pool
	cloudEnvironment string
	cfg              *config.Config
	configBase       *config.Config
	coll             *collector.Collector
	anal             *analyzer.Analyzer
	llmMgr           *llm.Manager
	exec             *executor.Executor
	actionStore      *store.ActionStore
	llmClient        *llm.Client
	// configRampStart is the raw trust.ramp_start timestamp parsed
	// from YAML at startup. Fleet-mode per-database bootstraps reuse
	// this value when seeding each database's sage.config row so
	// that YAML-configured ramp starts are not silently replaced by
	// now() at first run. Zero value means YAML had no override.
	configRampStart  time.Time
	fleetMgr         *fleet.DatabaseManager
	apiServer        *http.Server
	globalMetaState  *metaDBState
	configController *config.ConfigController
)

var (
	shutdownCtx         context.Context
	shutdownCancel      context.CancelFunc
	rateLimiterInstance *RateLimiter
)

// restartExitCode is the exit status that signals a supervisor (the
// launcher loop / orchestrator) to relaunch the process. Used by the
// /api/v1/restart endpoint so startup-only settings take effect.
const restartExitCode = 42

// sigCh receives OS signals and the in-process restart request.
var sigCh = make(chan os.Signal, 1)

// restartRequested is set when a restart (not a plain shutdown) was asked
// for, so the shutdown path exits with restartExitCode.
var restartRequested atomic.Bool

// triggerRestart asks the main loop to shut down and exit with the restart
// code. Safe to call from an HTTP handler goroutine.
func triggerRestart() {
	restartRequested.Store(true)
	select {
	case sigCh <- syscall.SIGTERM:
	default: // a shutdown is already in progress
	}
}

// fleetLLMBudget is the optional per-database LLM token budget (F5).
// nil when llm.fleet_token_budget_daily is 0.
var fleetLLMBudget *fleet.FleetBudget

// dbBudget adapts a per-database FleetBudget allocation to llm.Budgeter.
type dbBudget struct {
	b  *fleet.FleetBudget
	db string
}

func (d dbBudget) CanSpend(tokens int) bool { return d.b.CanSpend(d.db, tokens) }
func (d dbBudget) Spend(tokens int)         { d.b.Spend(d.db, tokens) }

func main() {
	runSubcommandAndExit()
	// Catch SIGINT/SIGTERM before anything starts: a signal during startup
	// (an orchestrator stopping the process right after it reported ready)
	// must shut down gracefully once startup is done, not kill the process.
	// sigCh is package-level so the /restart endpoint can trigger shutdown.
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	loadStartupConfigOrExit()

	// Meta-DB mode: connect to dedicated metadata database first.
	metaState, closeMetaPool := connectMetaDatabaseOrExit()
	defer closeMetaPool()
	// Standard mode: connect to monitored database directly.
	// Fleet mode creates its own per-database pools in initFleetMultiDB.
	closeMonitoredPool := connectStandaloneDatabaseOrExit()
	defer closeMonitoredPool()

	initProcessContext()
	initModeRuntime(metaState)

	// Construct the API rate limiter before initFleetAndAPI captures its
	// dependencies and starts the HTTP server.
	rateLimiterInstance = NewRateLimiter(cfg.RateLimit())

	// Fleet manager + REST API (wraps standalone or fleet instances).
	initFleetAndAPI()

	// Config hot-reload.
	stopConfigWatcher := startConfigWatcher()
	defer stopConfigWatcher()

	// Prometheus server.
	promServer := startPrometheusServer(cfg.Prometheus.ListenAddr)

	// Graceful shutdown.
	sig := <-sigCh
	shutdownProcess(sig, promServer)
}

// runSubcommandAndExit handles the vector-lab subcommand and --version,
// which exit without starting the sidecar.
func runSubcommandAndExit() {
	if len(os.Args) > 1 && os.Args[1] == "vector-lab" {
		os.Exit(runVectorLab())
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("pg_sage %s (commit: %s, built: %s, sql-ast: %s)\n",
			version, commit, date, sqlASTStatus())
		os.Exit(0)
	}
}

// loadStartupConfigOrExit loads the config, keeps the file baseline and
// installs the trusted-proxy list before any listener starts.
func loadStartupConfigOrExit() {
	var err error
	cfg, err = config.Load(os.Args[1:])
	if err != nil {
		logError("startup", "config: %v", err)
		os.Exit(1)
	}
	configBase = config.Clone(cfg)

	logInfo("startup", "pg_sage sidecar v%s — mode=%s", version, cfg.Mode)
	logInfo("startup", "Prometheus=%s API=%s",
		cfg.Prometheus.ListenAddr, cfg.API.ListenAddr)
	if !executor.ASTValidationAvailable() {
		logWarn("startup", "built without cgo: parse-tree SQL validation is "+
			"unavailable, so unattended changes wait for operator approval")
	}

	// Initialise the trusted-proxy net list from config. Empty config
	// falls back to loopback. Must run before any HTTP listener starts
	// so rate-limiter IP extraction is correct from the first request.
	setTrustedProxies(cfg.API.TrustedProxies)
}

// connectMetaDatabaseOrExit connects and initializes the metadata database
// when one is configured. The returned func closes its pool.
func connectMetaDatabaseOrExit() (*metaDBState, func()) {
	if !cfg.HasMetaDB() {
		return nil, func() {}
	}
	logInfo("startup", "connecting to meta database…")
	metaPool, metaErr := connectMetaDB(cfg.MetaDB)
	if metaErr != nil {
		logError("startup", "meta-db: %v", metaErr)
		os.Exit(1)
	}
	closeMetaPool := metaPool.Close

	state, initErr := initMetaDB(metaPool, cfg.EncryptionKey)
	if initErr != nil {
		logError("startup", "meta-db init: %v", initErr)
		os.Exit(1)
	}
	globalMetaState = state
	pool = metaPool
	logInfo("startup", "meta database initialized")
	return state, closeMetaPool
}

// connectStandaloneDatabaseOrExit opens the monitored database pool in
// standalone mode. The returned func closes it.
func connectStandaloneDatabaseOrExit() func() {
	if cfg.HasMetaDB() || cfg.IsFleet() {
		return func() {}
	}
	dsn := cfg.Postgres.DSN()
	if dsn == "" {
		dsn = envOrDefault("SAGE_DATABASE_URL",
			"postgres://postgres@localhost:5432/"+
				"postgres?sslmode=disable")
	}
	var err error
	pool, err = connectMonitoredDB(dsn, cfg.Postgres.MaxConnections)
	if err != nil {
		logError("startup", "%v", err)
		os.Exit(1)
	}
	closePool := pool.Close
	logInfo("startup", "connected to PostgreSQL")
	return closePool
}

// initProcessContext creates the shutdown context, starts the pool health
// check, detects the cloud environment and, for meta and fleet modes,
// builds the config controller.
func initProcessContext() {
	// Cancellable shutdown context for background goroutines.
	shutdownCtx, shutdownCancel = context.WithCancel(context.Background())

	// Background pool health.
	go poolHealthCheck()

	// Cloud environment detection.
	cloudEnvironment = detectCloudEnvironment()
	cfg.CloudEnvironment = cloudEnvironment
	logInfo("startup", "cloud environment: %s", cloudEnvironment)

	configRampStart = parseConfigRampStart(cfg.Trust.RampStart)
	if cfg.Trust.RampStart != "" && configRampStart.IsZero() {
		logWarn("startup", "could not parse trust.ramp_start %q, using now()",
			cfg.Trust.RampStart)
	}
	warnFastElevation(cfg, logWarn)
	if cfg.HasMetaDB() || cfg.IsFleet() {
		if err := initializeConfigController(pool); err != nil {
			logError("startup", "config controller: %v", err)
			os.Exit(1)
		}
	}
}

// initModeRuntime runs the mode-specific initialization, then makes sure a
// config controller exists.
func initModeRuntime(metaState *metaDBState) {
	if cfg.HasMetaDB() && metaState != nil {
		initMetaDBFleet(metaState)
	} else if cfg.IsStandalone() {
		initStandalone()
	} else if cfg.IsFleet() {
		initFleetMultiDB()
	}
	// Every mode needs a controller: the config watcher and API writes go
	// through it (a mode used to leave it nil, G5-B02).
	if err := ensureConfigController(); err != nil {
		logError("startup", "config controller: %v", err)
		os.Exit(1)
	}
}

// startConfigWatcher starts config hot-reload when a config file is in
// use. The returned func stops the watcher it started.
func startConfigWatcher() func() {
	if cfg.ConfigPath == "" {
		return func() {}
	}
	watcher := config.NewAcknowledgedWatcherWithLoader(
		cfg.ConfigPath, cfg, loadConfigCandidate, applyWatchedConfig,
	)
	if err := watcher.Start(); err != nil {
		logWarn("config", "hot-reload disabled: %v", err)
		return func() {}
	}
	return watcher.Stop
}

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

func startAPIServer(rl *RateLimiter) {
	if rl == nil {
		logError("api", "rate limiter unavailable; refusing to start API server")
		return
	}
	addr := cfg.API.ListenAddr
	if addr == "" {
		addr = ":8080"
	}

	configureAPIProcessHooks()
	result := wireRouter(WireParams{
		Cfg:       cfg,
		Pool:      pool,
		FleetMgr:  fleetMgr,
		LLMMgr:    llmMgr,
		MetaState: globalMetaState,
		Actions: struct {
			Store    *store.ActionStore
			Executor *executor.Executor
		}{
			Store:    actionStore,
			Executor: exec,
		},
		RateLimiter:      rl,
		Config:           configController,
		ConfigBase:       configBase,
		ConfigBaseLoader: loadFileConfigBase,
		LLMBudgets:       llmBudgetRegistry(),
		MCPHandler:       mcpHTTPHandler(),
	})
	startAuthPoolServices(result.AuthPool)
	serveAPI(addr, result.Handler)
}
