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
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
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
	configRampStart   time.Time
	fleetMgr          *fleet.DatabaseManager
	apiServer         *http.Server
	globalMetaState   *metaDBState
	configController  *config.ConfigController
	databricksAppAuth *auth.DatabricksAppConfig
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

// Hold and Settle make the database's share reset-aware (llm.HoldBudgeter).
func (d dbBudget) Hold(tokens int) (uint64, bool) { return d.b.Hold(d.db, tokens) }
func (d dbBudget) Settle(held, actual int, epoch uint64) {
	d.b.Settle(d.db, held, actual, epoch)
}

func main() {
	runSubcommandAndExit()
	// Catch SIGINT/SIGTERM before anything starts: a signal during startup
	// (an orchestrator stopping the process right after it reported ready)
	// must shut down gracefully once startup is done, not kill the process.
	// sigCh is package-level so the /restart endpoint can trigger shutdown.
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	loadStartupConfigOrExit()
	setHistoryProcessMode() // history_store_wiring.go

	// Meta-DB mode: connect to dedicated metadata database first.
	metaState, closeMetaPool := connectMetaDatabaseOrExit()
	defer closeMetaPool()
	// Standard mode: connect to monitored database directly.
	// Fleet mode creates its own per-database pools in initFleetMultiDB.
	closeMonitoredPool := connectStandaloneDatabaseOrExit()
	defer closeMonitoredPool()

	initProcessContext()
	if metaState != nil {
		startMetaDeferredIndexBuilder(shutdownCtx, pool) // config-04c
	}
	initModeRuntime(metaState)

	// Construct the API rate limiter before initFleetAndAPI captures its
	// dependencies and starts the HTTP server.
	rateLimiterInstance = NewRateLimiter(cfg.RateLimit())

	// Fleet manager + REST API (wraps standalone or fleet instances).
	initFleetAndAPI()

	// Config hot-reload.
	stopConfigWatcher := startConfigWatcher()
	defer stopConfigWatcher()
	// Global settings another replica saves converge here (config-05).
	startConfigResync(shutdownCtx)

	// Prometheus server.
	promServer := startPrometheusServer(cfg.Prometheus.ListenAddr)

	// Graceful shutdown.
	sig := <-sigCh
	shutdownProcess(sig, promServer)
}

// runSubcommandAndExit handles the vector-lab, bench, history, ledger,
// publish, evidence, verifier-backtest, lint-migration, plan, approve,
// apply, explain, config, pressure, doctor and healthcheck subcommands and
// --version, which exit without starting the sidecar.
func runSubcommandAndExit() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "vector-lab" {
		os.Exit(runVectorLab())
	}
	if len(os.Args) > 1 && os.Args[1] == "bench" {
		os.Exit(runBenchCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && telemetryDSNSubcommand(os.Args[1]) {
		os.Exit(runTelemetryDSNSubcommand(os.Args[1], os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == lintMigrationSubcommand {
		getenv, err := secretGetenv(lintDSNEnv)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "lint-migration: %v\n", err)
			os.Exit(lintExitToolError)
		}
		os.Exit(runLintMigration(context.Background(), os.Args[2:], getenv, os.Stdout,
			os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == evidenceSubcommand {
		os.Exit(runEvidenceCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == verifierBacktestSubcommand {
		os.Exit(runVerifierBacktestCommand(context.Background(), os.Args[2:], os.Stdout,
			os.Stderr))
	}
	if len(os.Args) > 1 && cliCommands[os.Args[1]] {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == pressureSubcommand {
		os.Exit(runPressureSubcommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == doctorSubcommand {
		os.Exit(runDoctorCommand(context.Background(), os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(versionLine())
		os.Exit(0)
	}
}

// runPressureSubcommand runs pressure, which talks to a running sidecar's
// API with the token in SAGE_API_TOKEN (or SAGE_API_TOKEN_FILE). plan and
// the other decision-loop commands go through run (cli_cmd.go).
func runPressureSubcommand(args []string) int {
	getenv, err := secretGetenv("SAGE_API_TOKEN")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s: %v\n", pressureSubcommand, err)
		return planExitUsage
	}
	return runPressureCommand(context.Background(), args, getenv, nil, os.Stdout, os.Stderr)
}

// telemetryDSNSubcommand reports the subcommands that read the monitored
// and meta DSNs: history, ledger and publish.
func telemetryDSNSubcommand(name string) bool {
	return name == "history" || name == ledgerSubcommand || name == publishSubcommand
}

// runTelemetryDSNSubcommand runs history, ledger or publish, which read the
// monitored and meta DSNs from SAGE_HISTORY_MONITORED_DSN and SAGE_META_DB.
func runTelemetryDSNSubcommand(name string, args []string) int {
	getenv, err := secretGetenv("SAGE_HISTORY_MONITORED_DSN", "SAGE_META_DB")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 2
	}
	if name == ledgerSubcommand {
		return runLedgerCommand(context.Background(), args, getenv, os.Stdout, os.Stderr)
	}
	if name == publishSubcommand {
		return runPublishCommand(context.Background(), args, defaultPublishDeps(getenv),
			os.Stdout, os.Stderr)
	}
	return runHistoryCommand(context.Background(), args, getenv, os.Stdout, os.Stderr)
}

// loadStartupConfigOrExit loads the config, keeps the file baseline and
// installs the trusted-proxy list before any listener starts.
func loadStartupConfigOrExit() {
	var authErr error
	databricksAppAuth, authErr = auth.LoadDatabricksAppConfigFromEnv(os.Getenv)
	if authErr != nil {
		logError("startup", "authentication configuration: %v", authErr)
		os.Exit(1)
	}
	var err error
	cfg, err = config.Load(os.Args[1:])
	if err != nil {
		logError("startup", "config: %v", err)
		os.Exit(1)
	}
	// Before any database opens: a generated key must be in place first.
	resolveEncryptionKeyOrExit()
	configBase = config.Clone(cfg)

	logInfo("startup", "pg_sage sidecar v%s — mode=%s", version, cfg.Mode)
	schema.SetBuildVersion(version) // recorded in sage.schema_migrations (config-04b)
	logInfo("startup", "Prometheus=%s API=%s",
		metricsWhere(cfg.Prometheus), cfg.API.ListenAddr)
	if sqlASTAssumed {
		logWarn("startup", "e2e TEST BUILD (%s): parse-tree SQL validation is "+
			"assumed, not present; never run this binary against a database "+
			"you care about", sqlASTAssumeTag)
	} else if !executor.ASTValidationAvailable() {
		logWarn("startup", "built without cgo: parse-tree SQL validation is "+
			"unavailable, so unattended changes wait for operator approval")
	}

	// Initialise the trusted-proxy net list from config. Empty config
	// falls back to loopback. Must run before any HTTP listener starts
	// so rate-limiter IP extraction is correct from the first request.
	setTrustedProxies(cfg.API.TrustedProxies)
	loadAPITLSOrExit()
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
	if err := initHistoryStoreSchema(metaPool); err != nil {
		logError("startup", "%v", err)
		os.Exit(1)
	}
	logInfo("startup", "meta database initialized")
	return state, closeMetaPool
}

// connectStandaloneDatabaseOrExit opens the monitored database pool in
// standalone mode. The returned func closes it.
func connectStandaloneDatabaseOrExit() func() {
	if cfg.HasMetaDB() || cfg.IsFleet() {
		return func() {}
	}
	var err error
	pool, err = connectMonitoredDB(
		cfg.Postgres.DSN(), cfg.Postgres.MaxConnections)
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
	// The TRUST and FAST ELEVATION lines come from initializeConfigController,
	// after persisted overrides, so they describe the effective values.
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
	warnDiscoveryOutsideMetaDB() // market-06: discovery enrols into sage.databases
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
