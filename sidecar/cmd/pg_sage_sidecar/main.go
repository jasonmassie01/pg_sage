package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/notify"
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
	if len(os.Args) > 1 && os.Args[1] == "vector-lab" {
		os.Exit(runVectorLab())
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("pg_sage %s (commit: %s, built: %s, sql-ast: %s)\n",
			version, commit, date, sqlASTStatus())
		os.Exit(0)
	}

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

	// Meta-DB mode: connect to dedicated metadata database first.
	var metaState *metaDBState
	if cfg.HasMetaDB() {
		logInfo("startup", "connecting to meta database…")
		metaPool, metaErr := connectMetaDB(cfg.MetaDB)
		if metaErr != nil {
			logError("startup", "meta-db: %v", metaErr)
			os.Exit(1)
		}
		defer metaPool.Close()

		state, initErr := initMetaDB(metaPool, cfg.EncryptionKey)
		if initErr != nil {
			logError("startup", "meta-db init: %v", initErr)
			os.Exit(1)
		}
		metaState = state
		globalMetaState = state
		pool = metaPool
		logInfo("startup", "meta database initialized")
	}

	// Standard mode: connect to monitored database directly.
	// Fleet mode creates its own per-database pools in initFleetMultiDB.
	if !cfg.HasMetaDB() && !cfg.IsFleet() {
		dsn := cfg.Postgres.DSN()
		if dsn == "" {
			dsn = envOrDefault("SAGE_DATABASE_URL",
				"postgres://postgres@localhost:5432/"+
					"postgres?sslmode=disable")
		}
		pool, err = connectMonitoredDB(dsn, cfg.Postgres.MaxConnections)
		if err != nil {
			logError("startup", "%v", err)
			os.Exit(1)
		}
		defer pool.Close()
		logInfo("startup", "connected to PostgreSQL")
	}

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
	if cfg.HasMetaDB() || cfg.IsFleet() {
		if err := initializeConfigController(pool); err != nil {
			logError("startup", "config controller: %v", err)
			os.Exit(1)
		}
	}

	// Mode-specific initialization.
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

	// Construct the API rate limiter before initFleetAndAPI captures its
	// dependencies and starts the HTTP server.
	rateLimiterInstance = NewRateLimiter(cfg.RateLimit())

	// Fleet manager + REST API (wraps standalone or fleet instances).
	initFleetAndAPI()

	// Config hot-reload.
	if cfg.ConfigPath != "" {
		watcher := config.NewAcknowledgedWatcherWithLoader(
			cfg.ConfigPath, cfg, loadConfigCandidate, applyWatchedConfig,
		)
		if err := watcher.Start(); err != nil {
			logWarn("config", "hot-reload disabled: %v", err)
		} else {
			defer watcher.Stop()
		}
	}

	// Prometheus server.
	promServer := startPrometheusServer(cfg.Prometheus.ListenAddr)

	// Graceful shutdown.
	// sigCh is package-level so the /restart endpoint can trigger shutdown.
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logInfo("shutdown", "received %s, shutting down…", sig)
	shutdownCancel()

	// Hard deadline: if graceful shutdown doesn't complete in 10s,
	// force exit so Ctrl+C never hangs indefinitely.
	go func() {
		time.Sleep(10 * time.Second)
		logError("shutdown", "timed out after 10s, forcing exit")
		os.Exit(forcedShutdownExitCode())
	}()

	if rateLimiterInstance != nil {
		rateLimiterInstance.Stop()
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(),
		8*time.Second)
	defer shutCancel()

	if err := promServer.Shutdown(shutCtx); err != nil {
		logWarn("shutdown", "Prometheus server: %v", err)
	}
	if apiServer != nil {
		if err := apiServer.Shutdown(shutCtx); err != nil {
			logWarn("shutdown", "API server: %v", err)
		}
	}
	// Stop the login rate-limiter's cleanup goroutine so it
	// doesn't outlive the API server.
	api.ShutdownLoginLimiter()

	// Drain executors so any rollback monitors started via
	// MonitorAndRollback finish cleanly (or time out) instead
	// of leaking. The per-executor Shutdown waits on its
	// internal WaitGroup, so we can run them in parallel.
	drainRuntimeWorkers(shutCtx)
	shutdownExecutors(shutCtx)

	logInfo("shutdown", "stopped")

	// If this was a restart request, exit with the restart code so the
	// supervisor (launcher loop / orchestrator) relaunches the process.
	if restartRequested.Load() {
		logInfo("shutdown", "restarting (exit %d)", restartExitCode)
		os.Exit(restartExitCode)
	}
}

func initializeConfigController(controlPool *pgxpool.Pool) error {
	if configController != nil {
		return nil
	}
	generation := uint64(1)
	if controlPool != nil {
		configStore := store.NewConfigStore(controlPool)
		var err error
		generation, err = configStore.GetGeneration(context.Background(), 0)
		if err != nil {
			return err
		}
		if err := applyPersistedGlobalOverrides(cfg, controlPool); err != nil {
			return err
		}
	}
	configController = config.NewConfigControllerAtGeneration(
		cfg, generation, nil, newTrustPolicyOwner(),
	)
	return nil
}

// ensureConfigController builds the controller for modes whose
// initialization did not (extension). It never replaces a live controller.
func ensureConfigController() error {
	if configController != nil {
		return nil
	}
	return initializeConfigController(configControlPool())
}

// applyWatchedConfig publishes a reloaded config.yaml candidate through the
// controller, persisting the generation when a control database exists.
func applyWatchedConfig(updated *config.Config) error {
	desired := configController.Desired()
	var result config.ApplyResult
	var applyErr error
	controlPool := configControlPool()
	if controlPool == nil {
		result, applyErr = configController.Apply(
			shutdownCtx, desired.Generation, updated,
		)
	} else {
		configStore := store.NewConfigStore(controlPool)
		result, applyErr = configController.ApplyWithPersistence(
			shutdownCtx, desired.Generation, updated,
			func(ctx context.Context, snapshot config.ConfigSnapshot) error {
				generation, err := configStore.SetOverridesCAS(
					ctx, nil, 0, 0, desired.Generation,
				)
				if err == nil && generation != snapshot.Generation {
					return fmt.Errorf("durable generation %d, controller %d",
						generation, snapshot.Generation)
				}
				return err
			},
		)
	}
	if applyErr != nil {
		return applyErr
	}
	logInfo("config", "hot-reload desired=%d active=%d pending_restart=%d",
		result.DesiredGeneration, result.ActiveGeneration,
		len(result.PendingRestart))
	return nil
}

func loadConfigCandidate() (*config.Config, error) {
	candidate, err := config.Load(os.Args[1:])
	if err != nil {
		return nil, err
	}
	controlPool := configControlPool()
	if err := applyPersistedGlobalOverrides(candidate, controlPool); err != nil {
		return nil, err
	}
	return candidate, nil
}

func configControlPool() *pgxpool.Pool {
	if globalMetaState != nil {
		return globalMetaState.Pool
	}
	// Only standalone owns its monitored database's sage schema. YAML fleet
	// has no stable control DB; meta mode persists through globalMetaState.
	if cfg != nil && cfg.IsStandalone() {
		return pool
	}
	return nil
}

func applyPersistedGlobalOverrides(
	candidate *config.Config, controlPool *pgxpool.Pool,
) error {
	if controlPool == nil {
		return nil
	}
	overrides, err := store.NewConfigStore(controlPool).GetOverrides(
		context.Background(), 0,
	)
	if err != nil {
		return fmt.Errorf("load persisted global overrides: %w", err)
	}
	for _, override := range overrides {
		api.ApplyConfigOverrideSnapshot(
			candidate, override.Key, override.Value,
		)
	}
	return nil
}

// drainRuntimeWorkers waits, within ctx, for every database runtime's
// workers (autonomy drain, provider observability, orchestrators) once
// shutdownCtx has been cancelled.
func drainRuntimeWorkers(ctx context.Context) {
	if fleetMgr == nil {
		return
	}
	for name, inst := range fleetMgr.Instances() {
		if inst.Workers != nil && !waitProviderWorkers(ctx, inst.Workers) {
			logWarn("shutdown", "db %q: runtime workers exceeded shutdown deadline", name)
		}
	}
}

// shutdownExecutors calls Shutdown on every registered executor
// (standalone + each fleet instance) in parallel, bounded by the
// supplied context.
func shutdownExecutors(ctx context.Context) {
	var execs []*executor.Executor
	if exec != nil {
		execs = append(execs, exec)
	}
	if fleetMgr != nil {
		for _, inst := range fleetMgr.Instances() {
			if inst.Executor != nil {
				execs = append(execs, inst.Executor)
			}
		}
	}
	if len(execs) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, e := range execs {
		wg.Add(1)
		go func(ex *executor.Executor) {
			defer wg.Done()
			if err := ex.Shutdown(ctx); err != nil {
				logWarn("shutdown",
					"executor: %v", err)
			}
		}(e)
	}
	wg.Wait()
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
// registerNotifySenders wires the built-in notification senders onto a
// dispatcher. Without it, the executor/analyzer event path dispatched to
// a sender-less dispatcher and every notification silently no-op'd with
// "no sender for type" (F1).
func registerNotifySenders(d *notify.Dispatcher) {
	policy := notificationTargetPolicy()
	d.RegisterSender(notify.NewSlackSenderWithPolicy(policy))
	d.RegisterSender(notify.NewEmailSenderWithPolicy(policy))
	d.RegisterSender(notify.NewPagerDutySender())
}

// buildAlertRoutes constructs channel instances and severity routing
// from the alerting config.
func buildAlertRoutes(
	c *config.Config,
	logFn func(string, string, ...any),
) map[string][]alerting.Channel {
	channels := make(map[string]alerting.Channel)
	if c.Alerting.SlackWebhookURL != "" {
		channels["slack"] = alerting.NewSlack(
			c.Alerting.SlackWebhookURL, logFn,
		)
	}
	if c.Alerting.PagerDutyRoutingKey != "" {
		channels["pagerduty"] = alerting.NewPagerDuty(
			c.Alerting.PagerDutyRoutingKey, logFn,
		)
	}
	for _, wh := range c.Alerting.Webhooks {
		channels["webhook:"+wh.Name] = alerting.NewWebhook(
			wh.Name, wh.URL, wh.Headers, logFn,
		)
	}

	routes := make(map[string][]alerting.Channel)
	for _, r := range c.Alerting.Routes {
		for _, chName := range r.Channels {
			if ch, ok := channels[chName]; ok {
				routes[r.Severity] = append(
					routes[r.Severity], ch,
				)
			}
		}
	}
	return routes
}

func initFleetAndAPI() {
	if fleetMgr == nil {
		fleetMgr = fleet.NewManager(cfg)
	}

	// Every mode's init registered its database runtimes.
	startMCPRuntime()

	startAPIServer(rateLimiterInstance)

	// Start session cleaner against the same canonical control pool that
	// owns authentication. Monitored pools are replaceable in meta mode.
	sessionPool := sessionControlPool(globalMetaState, fleetMgr, pool)
	if sessionPool != nil {
		go auth.StartSessionCleaner(
			shutdownCtx, sessionPool, time.Hour,
		)
	}
}

func sessionControlPool(
	metaState *metaDBState,
	mgr *fleet.DatabaseManager,
	fallback *pgxpool.Pool,
) *pgxpool.Pool {
	if metaState != nil && metaState.Pool != nil {
		return metaState.Pool
	}
	if mgr != nil {
		if fleetPool := mgr.PoolForDatabase("all"); fleetPool != nil {
			return fleetPool
		}
	}
	return fallback
}

// startInstanceWorker registers a goroutine with the database runtime before
// starting it, so removal can cancel and drain every instance-owned worker.
func startInstanceWorker(workers *sync.WaitGroup, run func()) {
	workers.Add(1)
	go func() {
		defer workers.Done()
		run()
	}()
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
// silenceSelfStats stops pg_stat_statements from recording pg_sage's own
// monitoring queries on this connection. Best-effort: failures (e.g. a
// non-superuser role on a managed provider) are ignored, leaving the
// /* pg_sage */ query tag and self-monitoring filter as the fallback.
func silenceSelfStats(ctx context.Context, c *pgx.Conn) error {
	_, _ = c.Exec(ctx, "SET pg_stat_statements.track = 'none'")
	return nil
}

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

func startAPIServer(rl *RateLimiter) {
	if rl == nil {
		logError("api", "rate limiter unavailable; refusing to start API server")
		return
	}
	addr := cfg.API.ListenAddr
	if addr == "" {
		addr = ":8080"
	}

	// Wire the process shutdown context into router-owned
	// goroutines (OAuth CSRF state cleaner) so they exit on
	// SIGINT/SIGTERM instead of leaking.
	api.SetShutdownContext(shutdownCtx)
	// Offer restart only under a declared supervisor; otherwise the API
	// answers 501 instead of exiting 42 into nothing (G5-B09).
	if supervisorDeclared(os.Getenv) {
		api.SetRestartFunc(triggerRestart)
	} else {
		logInfo("api", "restart endpoint disabled: set SAGE_SUPERVISED=1 "+
			"when a supervisor relaunches the sidecar on exit %d", restartExitCode)
	}

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

	// Fail loudly (not silently) when there is no usable auth pool.
	// Without it, registerAuthRoutes is skipped and every /api/* path
	// 401s with no /auth/login to recover — a bricked dashboard that
	// otherwise looks healthy in the logs (LIVE-01/04).
	if result.AuthPool == nil {
		logError("api",
			"AUTH DISABLED: no connected database available for session "+
				"storage — dashboard login and all authenticated API "+
				"endpoints are unavailable. Ensure at least one configured "+
				"database is reachable (or configure a meta-database), then "+
				"restart.")
	}

	// Start the agent-DB lifecycle reconciler: it archives expired leases
	// and destroys abandoned deployments. The logic was built and tested
	// but never scheduled (F4). Dormant when no agent DBs exist.
	if result.AuthPool != nil {
		startAgentDBReconciler(shutdownCtx, result.AuthPool)
	}

	apiServer = &http.Server{
		Addr:              addr,
		Handler:           result.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logInfo("api", "listening on %s", addr)
		if err := apiServer.ListenAndServe(); err != nil &&
			err != http.ErrServerClosed {
			logError("api", "server error: %v", err)
		}
	}()
}

// startAgentDBReconciler launches the periodic agent-DB lifecycle
// reconciler. interval <= 0 disables it.
func startAgentDBReconciler(ctx context.Context, pool *pgxpool.Pool) {
	interval := cfg.AgentDB.ReconcileIntervalSeconds
	if interval <= 0 {
		logInfo("agentdb", "lifecycle reconciler disabled (interval<=0)")
		return
	}
	store := agentdb.NewStore(pool)
	store.SetRequireBackupBeforeDestroy(cfg.AgentDB.RequireBackupBeforeDrop)
	store.SetMutationGate(func(c context.Context) error {
		return agentDBMutationGate(fleetMgr)(c) // read fleetMgr at call time
	})
	registry := agentdb.RuntimeRunnerRegistryFromEnv(ctx)
	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reconcileAgentDBsOnce(ctx, store, registry)
			}
		}
	}()
	logInfo("agentdb",
		"lifecycle reconciler started, interval=%ds", interval)
}

// reconcileAgentDBsOnce runs one reconcile pass (extracted for testing).
func reconcileAgentDBsOnce(
	ctx context.Context,
	store *agentdb.Store,
	registry *agentdb.RunnerRegistry,
) {
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := store.ReconcileAbandonedDeployments(
		rctx, time.Now(), registry)
	if err != nil {
		logWarn("agentdb", "reconcile abandoned: %v", err)
	} else if len(res.Archived) > 0 || len(res.DestroyLive) > 0 ||
		len(res.DestroyDryRun) > 0 || len(res.Blocked) > 0 {
		logInfo("agentdb",
			"reconcile: archived=%d destroyed=%d dry_run=%d blocked=%d",
			len(res.Archived), len(res.DestroyLive),
			len(res.DestroyDryRun), len(res.Blocked))
	}
	if _, err := store.ReconcileLiveProvisioning(rctx, registry); err != nil {
		logWarn("agentdb", "reconcile live provisioning: %v", err)
	}
	// Bring newly-provisioned agent databases into the fleet so the
	// collector monitors them (B1).
	if fleetMgr != nil {
		syncAgentDBsToFleet(rctx, store, fleetMgr)
	}
}

// resetFleetBudgetDaily resets the per-database LLM token budget at each
// UTC midnight (F5). A 24h ticker drifted with process start time.
func resetFleetBudgetDaily(ctx context.Context, b *fleet.FleetBudget) {
	for {
		timer := time.NewTimer(durationUntilNextUTCMidnight(time.Now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			b.ResetDaily()
		}
	}
}

// durationUntilNextUTCMidnight is strictly positive: at exactly midnight the
// next reset is 24h away.
func durationUntilNextUTCMidnight(now time.Time) time.Duration {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(utc)
}

func updateInstanceFindings(
	ctx context.Context,
	inst *fleet.DatabaseInstance,
) {
	refreshCollectionStatus(ctx, inst)
	dbPool := inst.Pool
	if dbPool == nil {
		return
	}
	rows, err := dbPool.Query(ctx,
		`SELECT severity, count(*)
		   FROM sage.findings
		  WHERE status = 'open'
		  GROUP BY severity`)
	if err != nil {
		logWarn("fleet", "db %q: findings query: %v", inst.Name, err)
		markInstanceConnectivity(ctx, inst, dbPool)
		return
	}
	defer rows.Close()

	var open, critical, warning, info int
	for rows.Next() {
		var sev string
		var cnt int
		if err := rows.Scan(&sev, &cnt); err != nil {
			continue
		}
		open += cnt
		switch sev {
		case "critical":
			critical = cnt
		case "warning":
			warning = cnt
		case "info":
			info = cnt
		}
	}
	now := time.Now()
	inst.UpdateStatus(func(s *fleet.InstanceStatus) {
		s.FindingsOpen = open
		s.FindingsCritical = critical
		s.FindingsWarning = warning
		s.FindingsInfo = info
		s.AnalyzerLastRun = now
		s.LastSeen = now
		s.Connected = true
		s.Error = ""
		if fleetLLMBudget != nil {
			s.LLMTokensUsed = fleetLLMBudget.Used(inst.Name)
		}
	})
}

// markInstanceConnectivity distinguishes a down database from a query
// error: only a failed ping flips the instance to disconnected, so a
// missing table does not trigger meta-mode reconnect churn (G5-B12).
func markInstanceConnectivity(
	ctx context.Context, inst *fleet.DatabaseInstance, dbPool *pgxpool.Pool,
) {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pingErr := dbPool.Ping(pingCtx)
	if pingErr == nil {
		return
	}
	logWarn("fleet", "db %q: unreachable: %v", inst.Name, pingErr)
	inst.UpdateStatus(func(s *fleet.InstanceStatus) {
		s.Connected = false
		s.Error = fmt.Sprintf("unreachable: %v", pingErr)
	})
}

// --- Prometheus ---

func startPrometheusServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handleMetrics)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		logInfo("prometheus", "listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logError("prometheus", "server error: %v", err)
		}
	}()
	return srv
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var b strings.Builder

	// Info metric.
	b.WriteString("# HELP pg_sage_info pg_sage version\n# TYPE pg_sage_info gauge\n")
	fmt.Fprintf(&b, "pg_sage_info{version=%q,mode=%q} 1\n\n", version, cfg.Mode)

	writeModeMetric(&b, cfg.Mode)

	// Connection metric.
	b.WriteString("# HELP pg_sage_connection_up PostgreSQL connection status\n# TYPE pg_sage_connection_up gauge\n")
	connUp := 0
	if pool != nil {
		if err := pool.Ping(ctx); err == nil {
			connUp = 1
		}
	} else if fleetMgr != nil {
		// Fleet mode: report up if any instance is connected.
		for _, inst := range fleetMgr.Instances() {
			if inst.Pool != nil {
				if err := inst.Pool.Ping(ctx); err == nil {
					connUp = 1
					break
				}
			}
		}
	}
	fmt.Fprintf(&b, "pg_sage_connection_up %d\n\n", connUp)

	// Standalone metrics.
	if cfg.IsStandalone() {
		writeStandaloneMetrics(&b, ctx)
	}

	// Fleet metrics.
	if cfg.Mode == "fleet" && fleetMgr != nil {
		writeFleetMetrics(&b)
	}
	writeFleetBudgetMetrics(&b, fleetLLMBudget)

	// Database metrics (only when global pool exists).
	if pool != nil {
		writeDatabaseMetrics(&b, ctx)
		writeValueMetrics(&b, ctx)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, b.String())
}

func writeStandaloneMetrics(b *strings.Builder, ctx context.Context) {
	// Findings from sage.findings table.
	b.WriteString("# HELP pg_sage_findings_total Open findings by severity\n# TYPE pg_sage_findings_total gauge\n")
	if anal != nil {
		counts := anal.OpenFindingsCount()
		for _, sev := range []string{"critical", "warning", "info"} {
			fmt.Fprintf(b, "pg_sage_findings_total{severity=%q} %d\n", sev, counts[sev])
		}
	}
	b.WriteString("\n")

	// Collector metrics.
	if coll != nil {
		snap := coll.LatestSnapshot()
		if snap != nil {
			b.WriteString("# HELP pg_sage_collector_last_run_timestamp Last collector run\n# TYPE pg_sage_collector_last_run_timestamp gauge\n")
			fmt.Fprintf(b, "pg_sage_collector_last_run_timestamp %d\n\n", snap.CollectedAt.Unix())
		}
	}

	// LLM metrics.
	if llmClient != nil {
		b.WriteString("# HELP pg_sage_llm_enabled LLM integration enabled\n# TYPE pg_sage_llm_enabled gauge\n")
		enabled := 0
		if llmClient.IsEnabled() {
			enabled = 1
		}
		fmt.Fprintf(b, "pg_sage_llm_enabled %d\n\n", enabled)

		b.WriteString("# HELP pg_sage_llm_circuit_open LLM circuit breaker (0=closed, 1=open)\n# TYPE pg_sage_llm_circuit_open gauge\n")
		circuitVal := 0
		if llmClient.IsCircuitOpen() {
			circuitVal = 1
		}
		fmt.Fprintf(b, "pg_sage_llm_circuit_open %d\n\n", circuitVal)

		b.WriteString("# HELP pg_sage_llm_tokens_used_today Tokens consumed today\n# TYPE pg_sage_llm_tokens_used_today gauge\n")
		fmt.Fprintf(b, "pg_sage_llm_tokens_used_today %d\n\n", llmClient.TokensUsedToday())

		b.WriteString("# HELP pg_sage_llm_tokens_budget_daily Daily token budget\n# TYPE pg_sage_llm_tokens_budget_daily gauge\n")
		fmt.Fprintf(b, "pg_sage_llm_tokens_budget_daily %d\n\n", cfg.LLM.TokenBudgetDaily)
	}

	// Optimizer metrics from sage.findings.
	writeOptimizerMetrics(b, ctx)
}

func writeOptimizerMetrics(b *strings.Builder, ctx context.Context) {
	b.WriteString("# HELP pg_sage_optimizer_recommendations_total Index recommendations by category\n")
	b.WriteString("# TYPE pg_sage_optimizer_recommendations_total gauge\n")

	rows, err := pool.Query(ctx,
		`SELECT category, count(*)
		 FROM sage.findings
		 WHERE status = 'open'
		   AND category IN ('missing_index','covering_index','partial_index','composite_index')
		 GROUP BY category`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var cat string
			var cnt int64
			if rows.Scan(&cat, &cnt) == nil {
				fmt.Fprintf(b, "pg_sage_optimizer_recommendations_total{category=%q} %d\n", cat, cnt)
			}
		}
	}
	b.WriteString("\n")

	b.WriteString("# HELP pg_sage_optimizer_enabled Optimizer v2 enabled\n")
	b.WriteString("# TYPE pg_sage_optimizer_enabled gauge\n")
	optEnabled := 0
	if cfg.LLM.Optimizer.Enabled {
		optEnabled = 1
	}
	fmt.Fprintf(b, "pg_sage_optimizer_enabled %d\n\n", optEnabled)
}

func writeFleetMetrics(b *strings.Builder) {
	status := fleetMgr.FleetStatus()
	b.WriteString("# HELP pg_sage_fleet_databases Total fleet databases\n# TYPE pg_sage_fleet_databases gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_databases %d\n\n",
		status.Summary.TotalDatabases)

	b.WriteString("# HELP pg_sage_fleet_healthy Healthy databases\n# TYPE pg_sage_fleet_healthy gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_healthy %d\n\n",
		status.Summary.Healthy)

	b.WriteString("# HELP pg_sage_fleet_findings_total Total open findings\n# TYPE pg_sage_fleet_findings_total gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_findings_total %d\n\n",
		status.Summary.TotalFindings)

	b.WriteString("# HELP pg_sage_fleet_findings_critical Total critical findings\n# TYPE pg_sage_fleet_findings_critical gauge\n")
	fmt.Fprintf(b, "pg_sage_fleet_findings_critical %d\n\n",
		status.Summary.TotalCritical)

	b.WriteString("# HELP pg_sage_fleet_instance_findings Per-instance open findings\n# TYPE pg_sage_fleet_instance_findings gauge\n")
	for _, db := range status.Databases {
		fmt.Fprintf(b,
			"pg_sage_fleet_instance_findings{database=%q} %d\n",
			db.Name, db.Status.FindingsOpen)
	}
	b.WriteString("\n")

	b.WriteString("# HELP pg_sage_fleet_instance_health Per-instance health score\n# TYPE pg_sage_fleet_instance_health gauge\n")
	for _, db := range status.Databases {
		fmt.Fprintf(b,
			"pg_sage_fleet_instance_health{database=%q} %d\n",
			db.Name, db.Status.HealthScore)
	}
	b.WriteString("\n")
}

func writeDatabaseMetrics(b *strings.Builder, ctx context.Context) {
	// Connections.
	b.WriteString("# HELP pg_sage_connections_total Connections by state\n# TYPE pg_sage_connections_total gauge\n")
	rows, err := pool.Query(ctx, `SELECT coalesce(state, 'unknown'), count(*) FROM pg_stat_activity GROUP BY state`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var state string
			var cnt int64
			if rows.Scan(&state, &cnt) == nil {
				fmt.Fprintf(b, "pg_sage_connections_total{state=%q} %d\n", state, cnt)
			}
		}
		b.WriteString("\n")
	}

	// Database size.
	var dbSize int64
	if pool.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&dbSize) == nil {
		b.WriteString("# HELP pg_sage_database_size_bytes Database size\n# TYPE pg_sage_database_size_bytes gauge\n")
		fmt.Fprintf(b, "pg_sage_database_size_bytes %d\n\n", dbSize)
	}

	// Cache hit ratio.
	var hit, read int64
	if pool.QueryRow(ctx, `SELECT blks_hit, blks_read FROM pg_stat_database WHERE datname = current_database()`).Scan(&hit, &read) == nil && (hit+read) > 0 {
		ratio := float64(hit) / float64(hit+read)
		b.WriteString("# HELP pg_sage_cache_hit_ratio Buffer cache hit ratio\n# TYPE pg_sage_cache_hit_ratio gauge\n")
		fmt.Fprintf(b, "pg_sage_cache_hit_ratio %g\n\n", ratio)
	}
}

// --- Rate limiter ---

type RateLimiter struct {
	mu       sync.Mutex
	windows  map[string][]time.Time
	limit    int
	interval time.Duration
	stop     chan struct{}
	stopOnce sync.Once
}

func NewRateLimiter(maxPerMinute int) *RateLimiter {
	if maxPerMinute <= 0 {
		maxPerMinute = config.DefaultRateLimit
	}
	rl := &RateLimiter{
		windows:  make(map[string][]time.Time),
		limit:    maxPerMinute,
		interval: time.Minute,
		stop:     make(chan struct{}),
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) Stop() {
	rl.stopOnce.Do(func() { close(rl.stop) })
}

func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-rl.interval)
	ts := rl.windows[ip]
	start := 0
	for start < len(ts) && ts[start].Before(cutoff) {
		start++
	}
	ts = ts[start:]
	if len(ts) >= rl.limit {
		rl.windows[ip] = ts
		return false
	}
	rl.windows[ip] = append(ts, now)
	return true
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			rl.evictExpired(now)
		case <-rl.stop:
			return
		}
	}
}

func (rl *RateLimiter) evictExpired(now time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := now.Add(-rl.interval)
	for ip, ts := range rl.windows {
		start := 0
		for start < len(ts) && ts[start].Before(cutoff) {
			start++
		}
		if start >= len(ts) {
			delete(rl.windows, ip)
		} else {
			rl.windows[ip] = ts[start:]
		}
	}
}

func rateLimitMiddleware(rl *RateLimiter, next http.Handler) http.Handler {
	if rl == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(clientIP(r)) {
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// defaultTrustedProxies is used when cfg.API.TrustedProxies is empty:
// X-Forwarded-For is only honoured from loopback.
var defaultTrustedProxies = []string{"127.0.0.1", "::1"}

// trustedProxyNets caches the parsed *net.IPNet list derived from
// cfg.API.TrustedProxies. Rebuilt whenever cfg is reloaded. Guarded by
// the config hot-reload lock at build time.
var (
	trustedProxyNets   []*net.IPNet
	trustedProxyNetsMu sync.RWMutex
)

// buildTrustedProxyNets parses the configured trusted-proxies list
// (plain IPs or CIDR blocks) into []*net.IPNet for O(1) matching.
// Unparseable entries are logged and skipped. Called once during
// bootstrap and from the config hot-reload path.
func buildTrustedProxyNets(entries []string) []*net.IPNet {
	if len(entries) == 0 {
		entries = defaultTrustedProxies
	}
	var nets []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.Contains(e, "/") {
			// Plain IP → /32 or /128.
			ip := net.ParseIP(e)
			if ip == nil {
				logWarn("config",
					"trusted_proxies: ignoring invalid IP %q", e)
				continue
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			e = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			logWarn("config",
				"trusted_proxies: ignoring invalid CIDR %q: %v",
				e, err)
			continue
		}
		nets = append(nets, n)
	}
	return nets
}

// setTrustedProxies publishes a freshly parsed list. Safe for
// concurrent reads from clientIP.
func setTrustedProxies(entries []string) {
	nets := buildTrustedProxyNets(entries)
	trustedProxyNetsMu.Lock()
	trustedProxyNets = nets
	trustedProxyNetsMu.Unlock()
}

// isTrustedProxy reports whether host (an IP literal) is in the
// configured trusted-proxies list.
func isTrustedProxy(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	trustedProxyNetsMu.RLock()
	defer trustedProxyNetsMu.RUnlock()
	for _, n := range trustedProxyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	remoteIP := net.ParseIP(host)
	if remoteIP == nil {
		return host
	}
	remote := remoteIP.String()

	// Only trust X-Forwarded-For when the immediate peer is in the
	// configured trusted-proxies list. Spoofed XFF from a direct
	// attacker is ignored, preserving per-IP rate limits.
	if !isTrustedProxy(remote) {
		return remote
	}
	xff := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if xff == "" {
		return remote
	}
	return forwardedClientIP(xff, remote)
}

// forwardedClientIP walks the proxy chain from the server back toward the
// client. The first untrusted hop is the client; entries before it are
// client-controlled and intentionally ignored. A malformed trusted-side hop
// invalidates the header and falls back to the immediate peer.
func forwardedClientIP(xff, remote string) string {
	parts := strings.Split(xff, ",")
	candidate := remote
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			return remote
		}
		candidate = ip.String()
		if !isTrustedProxy(candidate) {
			return candidate
		}
	}
	return candidate
}

// --- Detection ---

func detectCloudEnvironment() string {
	return detectCloudEnv(pool)
}

// detectCloudEnv probes a connection pool for managed service
// indicators. Pass any per-database pool in fleet mode.
func detectCloudEnv(p *pgxpool.Pool) string {
	if p == nil {
		return "unknown"
	}
	if provider := hostedProviderFromHost(p.Config().ConnConfig.Host); provider != "" {
		return provider
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var s string
	if p.QueryRow(ctx, "SELECT aurora_version()").Scan(&s) == nil {
		return "aurora"
	}
	var ps *string
	if p.QueryRow(ctx, "SELECT current_setting('rds.extensions', true)").Scan(&ps) == nil && ps != nil {
		return "rds"
	}
	if p.QueryRow(ctx, "SELECT current_setting('alloydb.iam_authentication', true)").Scan(&ps) == nil && ps != nil {
		return "alloydb"
	}
	if p.QueryRow(ctx, "SELECT current_setting('cloudsql.iam_authentication', true)").Scan(&ps) == nil && ps != nil {
		return "cloud-sql"
	}
	if p.QueryRow(ctx, "SELECT current_setting('azure.extensions', true)").Scan(&ps) == nil && ps != nil {
		return "azure"
	}
	return "self-managed"
}

func poolHealthCheck() {
	if pool == nil {
		return // fleet mode: no global pool to health-check
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(shutdownCtx, 5*time.Second)
			if err := pool.Ping(ctx); err != nil {
				logWarn("pool-health", "ping failed: %v", err)
			}
			cancel()
			stat := pool.Stat()
			if stat.TotalConns() == stat.MaxConns() && stat.IdleConns() == 0 {
				logWarn("pool-health", "exhausted — total=%d max=%d",
					stat.TotalConns(), stat.MaxConns())
			}
		case <-shutdownCtx.Done():
			return
		}
	}
}

// bootstrapAdminIfEmpty creates the first admin user when no users
// exist. Prints credentials to stdout so the operator can log in.
func bootstrapAdminIfEmpty(
	ctx context.Context, p *pgxpool.Pool,
) error {
	count, err := auth.UserCount(ctx, p)
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
		ctx, p, adminEmail, password,
	); err != nil {
		return fmt.Errorf("creating admin: %w", err)
	}
	logInfo("startup",
		"first admin created — email: %s  password: [redacted, see stderr]",
		adminEmail)
	fmt.Fprintf(os.Stderr, "\n*** INITIAL ADMIN PASSWORD: %s ***\n*** Change this password immediately. ***\n\n", password)
	return nil
}

// parseConfigRampStart parses cfg.Trust.RampStart (accepted in
// RFC3339 or date-only form) into a time.Time. Returns the zero
// time on empty input or unparseable values — callers may use
// IsZero() to distinguish "not set" from a valid timestamp.
func parseConfigRampStart(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339, "2006-01-02", "2006-01-02T15:04:05",
	} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func logInfo(component, msg string, args ...any)  { logStructured("INFO", component, msg, args...) }
func logWarn(component, msg string, args ...any)  { logStructured("WARN", component, msg, args...) }
func logError(component, msg string, args ...any) { logStructured("ERROR", component, msg, args...) }

func logStructured(level, component, msg string, args ...any) {
	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(os.Stderr, "%s [%s] [%s] %s\n", ts, level, component, fmt.Sprintf(msg, args...))
}

func logStructuredWrapper(component, msg string, args ...any) {
	switch level := strings.ToUpper(component); level {
	case "DEBUG", "INFO", "WARN", "ERROR":
		logStructured(level, "sidecar", msg, args...)
		return
	}
	logStructured("INFO", component, msg, args...)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
