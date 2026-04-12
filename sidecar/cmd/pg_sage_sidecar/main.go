package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/forecaster"
	"github.com/pg-sage/sidecar/internal/ha"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/retention"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/tuner"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/startup"
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
	analyzeSem         chan struct{}
	pool               *pgxpool.Pool
	extensionAvailable bool
	cloudEnvironment   string
	cfg                *config.Config
	coll               *collector.Collector
	anal               *analyzer.Analyzer
	adv                *advisor.Advisor
	llmMgr             *llm.Manager
	exec               *executor.Executor
	actionStore        *store.ActionStore
	haMon              *ha.Monitor
	briefWorker        *briefing.Worker
	llmClient          *llm.Client
	cleaner            *retention.Cleaner
	alertMgr           *alerting.Manager
	rampStart          time.Time
	// configRampStart is the raw trust.ramp_start timestamp parsed
	// from YAML at startup. Fleet-mode per-database bootstraps reuse
	// this value when seeding each database's sage.config row so
	// that YAML-configured ramp starts are not silently replaced by
	// now() at first run. Zero value means YAML had no override.
	configRampStart time.Time
	shutdownFlag       bool
	fleetMgr           *fleet.DatabaseManager
	apiServer          *http.Server
	globalMetaState    *metaDBState
)

var (
	shutdownCtx        context.Context
	shutdownCancel     context.CancelFunc
	rateLimiterInstance *RateLimiter
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("pg_sage %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	}

	var err error
	cfg, err = config.Load(os.Args[1:])
	if err != nil {
		logError("startup", "config: %v", err)
		os.Exit(1)
	}

	logInfo("startup", "pg_sage sidecar v%s — mode=%s", version, cfg.Mode)
	logInfo("startup", "Prometheus=%s API=%s",
		cfg.Prometheus.ListenAddr, cfg.API.ListenAddr)

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

	// Extension detection.
	extensionAvailable = detectExtension()
	if extensionAvailable {
		logInfo("startup", "mode: EXTENSION — pg_sage C extension detected")
	} else {
		logInfo("startup", "mode: SIDECAR — no extension, using catalog queries")
	}

	// Mode-specific initialization.
	if cfg.HasMetaDB() && metaState != nil {
		initMetaDBFleet(metaState)
	} else if cfg.IsStandalone() {
		initStandalone()
	} else if cfg.IsFleet() {
		initFleetMultiDB()
	}

	// Fleet manager + REST API (wraps standalone or fleet instances).
	initFleetAndAPI()

	// Config hot-reload.
	if cfg.ConfigPath != "" {
		watcher := config.NewWatcher(cfg.ConfigPath, cfg, func(updated *config.Config) {
			cfg = updated
			logInfo("config", "hot-reload applied")
		})
		if err := watcher.Start(); err != nil {
			logWarn("config", "hot-reload disabled: %v", err)
		} else {
			defer watcher.Stop()
		}
	}

	// Rate limiter.
	rl := NewRateLimiter(cfg.RateLimit())
	rateLimiterInstance = rl

	// Prometheus server.
	promServer := startPrometheusServer(cfg.Prometheus.ListenAddr)

	// Graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logInfo("shutdown", "received %s, shutting down…", sig)
	shutdownFlag = true
	shutdownCancel()

	// Hard deadline: if graceful shutdown doesn't complete in 10s,
	// force exit so Ctrl+C never hangs indefinitely.
	go func() {
		time.Sleep(10 * time.Second)
		logError("shutdown", "timed out after 10s, forcing exit")
		os.Exit(1)
	}()

	if rateLimiterInstance != nil {
		rateLimiterInstance.Stop()
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(),
		8*time.Second)
	defer shutCancel()

	// Release advisory lock if standalone.
	if cfg.IsStandalone() {
		schema.ReleaseAdvisoryLock(shutCtx, pool)
		logInfo("shutdown", "advisory lock released")
	}

	if err := promServer.Shutdown(shutCtx); err != nil {
		logWarn("shutdown", "Prometheus server: %v", err)
	}
	if apiServer != nil {
		if err := apiServer.Shutdown(shutCtx); err != nil {
			logWarn("shutdown", "API server: %v", err)
		}
	}
	logInfo("shutdown", "stopped")
}

func initStandalone() {
	ctx := context.Background()

	// 1. Prerequisite checks.
	logInfo("startup", "running prerequisite checks…")
	checks, err := startup.RunChecks(ctx, pool)
	if err != nil {
		logError("startup", "prerequisite check failed: %v", err)
		os.Exit(1)
	}
	cfg.PGVersionNum = checks.PGVersionNum
	cfg.HasWALColumns = checks.HasWALColumns
	cfg.HasPlanTimeColumns = checks.HasPlanTimeColumns
	logInfo("startup",
		"PG version: %d, WAL columns: %v, plan_time columns: %v, query text: %v",
		checks.PGVersionNum, checks.HasWALColumns,
		checks.HasPlanTimeColumns, checks.QueryTextVisible)
	if !checks.QueryTextVisible {
		logWarn("startup", "query text not visible — GRANT pg_read_all_stats TO %s", cfg.Postgres.User)
	}

	// 2. Schema bootstrap.
	logInfo("startup", "bootstrapping schema…")
	if err := schema.Bootstrap(ctx, pool); err != nil {
		logError("startup", "schema bootstrap: %v", err)
		os.Exit(1)
	}
	if err := schema.MigrateConfigSchema(ctx, pool); err != nil {
		logError("startup", "config schema migration: %v", err)
		os.Exit(1)
	}

	// 2a. Bootstrap admin user if none exist.
	if err := bootstrapAdminIfEmpty(ctx, pool); err != nil {
		logWarn("startup", "admin bootstrap: %v", err)
	}

	// 2b. Detect auto_explain availability.
	var autoExplainAvail *autoexplain.Availability
	if cfg.AutoExplain.Enabled {
		autoExplainAvail, err = autoexplain.Detect(ctx, pool)
		if err != nil {
			logWarn("startup", "auto_explain detection: %v", err)
		} else if autoExplainAvail.Available {
			logInfo("startup",
				"auto_explain available via %s",
				autoExplainAvail.Method)
		} else {
			if hint := autoexplain.EnableHint(cloudEnvironment); hint != "" {
				logWarn("startup",
					"auto_explain not available — to enable on %s: %s. "+
						"Using catalog-based plan sources until enabled.",
					cloudEnvironment, hint)
			} else {
				logInfo("startup", "auto_explain not available, using fallback plan sources")
			}
		}
	}

	// 3. Persist trust ramp start.
	configRampStart = parseConfigRampStart(cfg.Trust.RampStart)
	if cfg.Trust.RampStart != "" && configRampStart.IsZero() {
		logWarn("startup", "could not parse trust.ramp_start %q, using now()",
			cfg.Trust.RampStart)
	}
	rampStart, err = schema.PersistTrustRampStart(ctx, pool, configRampStart)
	if err != nil {
		logWarn("startup", "trust ramp start: %v", err)
		rampStart = time.Now()
	}
	logInfo("startup", "trust ramp start: %s (age: %s)",
		rampStart.Format(time.RFC3339), time.Since(rampStart).Round(time.Hour))

	// 3c. Fleet-wide ANALYZE semaphore — bounds parallel
	// ANALYZE execution across every Executor instance.
	maxAnalyze := cfg.Tuner.MaxConcurrentAnalyze
	if maxAnalyze <= 0 {
		maxAnalyze = 1
	}
	analyzeSem = make(chan struct{}, maxAnalyze)
	logInfo("startup",
		"ANALYZE semaphore sized to %d concurrent slots",
		maxAnalyze)

	// 4. Verify grants.
	executor.VerifyGrants(ctx, pool, cfg.Postgres.User, logStructuredWrapper)
	if cfg.Trust.Level == "autonomous" && cfg.Trust.Tier3Moderate && cfg.Trust.MaintenanceWindow == "" {
		logWarn("startup", "tier3_moderate enabled without maintenance_window — moderate actions will NOT execute")
	}

	// 5. HA monitor.
	haMon = ha.New(pool, logStructuredWrapper)

	// 6. LLM client.
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	llmMgr = llm.NewManager(llmClient, nil, false)

	// 7. Start collector.
	coll = collector.New(pool, cfg, cfg.PGVersionNum, logStructuredWrapper)
	go coll.Run(shutdownCtx)

	// 8. Start analyzer with v2 index optimizer.
	var opt *optimizer.Optimizer
	if cfg.LLM.Optimizer.Enabled {
		optClient := llmClient
		if cfg.LLM.OptimizerLLM.Enabled {
			optClient = llm.NewOptimizerClient(
				&cfg.LLM, &cfg.LLM.OptimizerLLM, logStructuredWrapper,
			)
			logInfo("startup", "optimizer using dedicated LLM model")
		}
		if optClient.IsEnabled() {
			var fallback *llm.Client
			if cfg.LLM.OptimizerLLM.FallbackToGeneral &&
				optClient != llmClient {
				fallback = llmClient
			}
			var optOpts []func(*optimizer.Optimizer)
			if autoExplainAvail != nil && autoExplainAvail.Available {
				optOpts = append(optOpts, optimizer.WithAutoExplain())
			}
			opt = optimizer.New(
				optClient, fallback, pool, &cfg.LLM.Optimizer,
				cfg.PGVersionNum, extensionAvailable,
				cfg.LLM.OptimizerLLM.MaxOutputTokens,
				logStructuredWrapper,
				optOpts...,
			)
			logInfo("startup", "index optimizer v2 enabled (plan_source=%s)",
				cfg.LLM.Optimizer.PlanSource)
		}
	}
	if cfg.Advisor.Enabled && llmClient.IsEnabled() {
		adv = advisor.New(pool, cfg, coll, llmMgr, logStructuredWrapper)
		logInfo("startup", "advisor enabled — interval=%s", cfg.Advisor.Interval())
	}
	var advIface analyzer.ConfigAdvisor
	if adv != nil {
		advIface = adv
	}

	// Forecaster.
	var fc *forecaster.Forecaster
	if cfg.Forecaster.Enabled {
		fcCfg := forecaster.ForecasterConfig{
			Enabled:              cfg.Forecaster.Enabled,
			LookbackDays:         cfg.Forecaster.LookbackDays,
			DiskWarnGrowthGBDay:  cfg.Forecaster.DiskWarnGrowthGBDay,
			ConnectionWarnPct:    cfg.Forecaster.ConnectionWarnPct,
			CacheWarnThreshold:   cfg.Forecaster.CacheWarnThreshold,
			SequenceWarnDays:     cfg.Forecaster.SequenceWarnDays,
			SequenceCriticalDays: cfg.Forecaster.SequenceCriticalDays,
		}
		fc = forecaster.New(pool, fcCfg, logStructuredWrapper)
		logInfo("startup", "forecaster enabled, lookback=%dd",
			cfg.Forecaster.LookbackDays)
	}
	var qt *tuner.Tuner
	if cfg.Tuner.Enabled {
		hpAvail, hpErr := tuner.DetectHintPlan(ctx, pool)
		if hpErr != nil {
			logWarn("startup",
				"pg_hint_plan detection: %v", hpErr)
		}
		if hpAvail != nil && hpAvail.Available {
			logInfo("startup",
				"pg_hint_plan available (%s), "+
					"hint table ready: %v",
				hpAvail.Method, hpAvail.HintTableReady)
		} else {
			logInfo("startup",
				"pg_hint_plan not available, "+
					"tuner runs in advisory mode")
		}
		tunerCfg := tuner.TunerConfig{
			Enabled:                cfg.Tuner.Enabled,
			LLMEnabled:             cfg.Tuner.LLMEnabled,
			WorkMemMaxMB:           cfg.Tuner.WorkMemMaxMB,
			PlanTimeRatio:          cfg.Tuner.PlanTimeRatio,
			NestedLoopRowThreshold: cfg.Tuner.NestedLoopRowThreshold,
			ParallelMinTableRows:   cfg.Tuner.ParallelMinTableRows,
			MinQueryCalls:          cfg.Tuner.MinQueryCalls,
			VerifyAfterApply:       cfg.Tuner.VerifyAfterApply,
			CascadeCooldownCycles:  cfg.Trust.CascadeCooldownCycles,

			// v0.8.5 Feature 1 — Hint revalidation loop.
			HintRetirementDays:           cfg.Tuner.HintRetirementDays,
			RevalidationIntervalHours:    cfg.Tuner.RevalidationIntervalHours,
			RevalidationKeepRatio:        cfg.Tuner.RevalidationKeepRatio,
			RevalidationRollbackRatio:    cfg.Tuner.RevalidationRollbackRatio,
			RevalidationExplainTimeoutMs: cfg.Tuner.RevalidationExplainTimeoutMs,

			// v0.8.5 Feature 2 — Stale-stats detection + ANALYZE.
			StaleStatsEstimateSkew:        cfg.Tuner.StaleStatsEstimateSkew,
			StaleStatsModRatio:            cfg.Tuner.StaleStatsModRatio,
			StaleStatsAgeMinutes:          cfg.Tuner.StaleStatsAgeMinutes,
			AnalyzeMaxTableMB:             cfg.Tuner.AnalyzeMaxTableMB,
			AnalyzeCooldownMinutes:        cfg.Tuner.AnalyzeCooldownMinutes,
			AnalyzeMaintenanceThresholdMB: cfg.Tuner.AnalyzeMaintenanceThresholdMB,
			AnalyzeTimeoutMs:              cfg.Tuner.AnalyzeTimeoutMs,
			MaxConcurrentAnalyze:          cfg.Tuner.MaxConcurrentAnalyze,
		}
		var tunerOpts []tuner.Option
		if cfg.Tuner.LLMEnabled && llmMgr != nil {
			tc := llmMgr.ForPurpose("query_tuning")
			var fb *llm.Client
			if cfg.LLM.OptimizerLLM.FallbackToGeneral &&
				llmMgr.General != nil {
				fb = llmMgr.General
			}
			tunerOpts = append(tunerOpts,
				tuner.WithLLM(tc, fb))
			logInfo("startup",
				"tuner LLM-enhanced mode enabled "+
					"(uses optimizer_llm)")
		}
		qt = tuner.New(pool, tunerCfg, hpAvail,
			logStructuredWrapper, tunerOpts...)
		logInfo("startup", "tuner enabled")

		// Start hint revalidation loop (v0.8.5 Feature 1).
		go qt.StartRevalidationLoop(
			shutdownCtx, tunerCfg.RevalidationIntervalHours)
	}

	anal = analyzer.New(
		pool, cfg, coll, opt, advIface, fc, qt,
		logStructuredWrapper,
	)
	go anal.Run(shutdownCtx)

	// 9. Executor runs after analyzer (called from analyzer loop).
	exec = executor.New(pool, cfg, anal, rampStart, logStructuredWrapper)
	exec.WithAnalyzeSemaphore(analyzeSem)

	// 9b. Action queue store + execution mode.
	actionStore = store.NewActionStore(pool)
	exec.WithActionStore(actionStore, resolveExecutionMode())

	// 9c. Notification dispatcher.
	notifyDispatcher := notify.NewDispatcher(
		pool, logStructuredWrapper)
	dbName := resolveDBName()
	exec.WithDispatcher(notifyDispatcher)
	exec.WithDatabaseName(dbName)
	anal.WithDispatcher(notifyDispatcher)
	anal.WithDatabaseName(dbName)

	go store.StartActionExpiry(shutdownCtx, actionStore, logStructuredWrapper)

	// 10. Briefing worker.
	briefWorker = briefing.New(pool, cfg, llmClient, logStructuredWrapper)

	// 10b. Alert manager.
	if cfg.Alerting.Enabled {
		routes := buildAlertRoutes(cfg, logStructuredWrapper)
		mcfg := alerting.ManagerConfig{
			CheckIntervalSeconds: cfg.Alerting.CheckIntervalSeconds,
			CooldownMinutes:      cfg.Alerting.CooldownMinutes,
			QuietHoursStart:      cfg.Alerting.QuietHoursStart,
			QuietHoursEnd:        cfg.Alerting.QuietHoursEnd,
			Timezone:             cfg.Alerting.Timezone,
		}
		alertMgr = alerting.New(pool, mcfg, routes, logStructuredWrapper)
		go alertMgr.Run(shutdownCtx)
		logInfo("startup", "alerting enabled, check_interval=%ds",
			cfg.Alerting.CheckIntervalSeconds)
	}

	// 10c. auto_explain collector — starts even without the
	// auto_explain extension; plain EXPLAIN still captures
	// plans for non-parameterized queries.
	if cfg.AutoExplain.Enabled {
		if autoExplainAvail == nil {
			autoExplainAvail = &autoexplain.Availability{}
		}
		aeCfg := autoexplain.CollectorConfig{
			CollectIntervalSeconds: cfg.AutoExplain.CollectIntervalSeconds,
			MaxPlansPerCycle:       cfg.AutoExplain.MaxPlansPerCycle,
			LogMinDurationMs:       cfg.AutoExplain.LogMinDurationMs,
			PreferSessionLoad:      cfg.AutoExplain.PreferSessionLoad,
		}
		aec := autoexplain.NewCollector(
			pool, aeCfg, autoExplainAvail, logStructuredWrapper,
		)
		go aec.Run(shutdownCtx)
		logInfo("startup",
			"auto_explain collector started, interval=%ds",
			cfg.AutoExplain.CollectIntervalSeconds)
	}

	// 11. Retention cleaner.
	cleaner = retention.New(pool, cfg, logStructuredWrapper)

	// 12. Start orchestrator goroutine — runs executor + retention after each analyzer cycle.
	go standaloneOrchestrator()

	logInfo("startup", "standalone mode initialized — collector=%ds, analyzer=%ds, trust=%s",
		cfg.Collector.IntervalSeconds, cfg.Analyzer.IntervalSeconds, cfg.Trust.Level)
}

func standaloneOrchestrator() {
	// Run executor and retention after each analyzer interval.
	ticker := time.NewTicker(cfg.Analyzer.Interval() + 5*time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if shutdownFlag {
				return
			}
			ctx := shutdownCtx

			// HA check.
			isReplica := false
			if haMon != nil {
				isReplica = haMon.Check(ctx)
			}

			// Executor.
			if exec != nil {
				exec.RunCycle(ctx, isReplica)
			}

			// Briefing (scheduled).
			if briefWorker != nil && briefWorker.ShouldRun(time.Now()) {
				text, bErr := briefWorker.Generate(ctx)
				if bErr != nil {
					logWarn("briefing", "generation failed: %v", bErr)
				} else {
					briefWorker.Dispatch(text)
					briefWorker.MarkRan()
				}
			}

			// Retention.
			if cleaner != nil {
				cleaner.Run(ctx)
			}

			// Update fleet status after each cycle.
			if fleetMgr != nil {
				updateFleetStatus(ctx)
			}
		}
	}
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

	// In standalone mode, register the single global instance.
	if cfg.IsStandalone() {
		dbName := resolveDBName()
		dbCfg := buildDBConfig(dbName)
		inst := &fleet.DatabaseInstance{
			Name:      dbName,
			Config:    dbCfg,
			Pool:      pool,
			Collector: coll,
			Analyzer:  anal,
			Executor:  exec,
			Status: &fleet.InstanceStatus{
				Connected:  true,
				PGVersion:  pgVersionString(cfg.PGVersionNum),
				TrustLevel: cfg.Trust.Level,
				LastSeen:   time.Now(),
			},
		}
		fleetMgr.RegisterInstance(inst)
	}
	// Fleet instances are already registered by initFleetMultiDB.

	startAPIServer(rateLimiterInstance)

	// Start session cleaner goroutine (cleans expired sessions hourly).
	sessionPool := fleetMgr.PoolForDatabase("all")
	if sessionPool == nil {
		sessionPool = pool
	}
	if sessionPool != nil {
		go auth.StartSessionCleaner(
			shutdownCtx, sessionPool, time.Hour,
		)
	}
}

// initFleetMultiDB creates per-database pools, collectors, analyzers,
// and executors for each database in fleet config.
func initFleetMultiDB() {
	fleetMgr = fleet.NewManager(cfg)

	// LLM client + manager (shared across fleet).
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	llmMgr = llm.NewManager(llmClient, nil, false)

	var configPool *pgxpool.Pool // first connected DB used for config store

	for i, dbCfg := range cfg.Databases {
		name := dbCfg.Name
		dsn := dbCfg.ConnString()
		logInfo("fleet", "connecting to database %q", name)

		poolCfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			logError("fleet", "db %q: invalid DSN: %v", name, err)
			fleetMgr.RegisterInstance(&fleet.DatabaseInstance{
				Name:   name,
				Config: dbCfg,
				Status: &fleet.InstanceStatus{
					Error:    fmt.Sprintf("invalid DSN: %v", err),
					LastSeen: time.Now(),
				},
			})
			continue
		}
		maxConns := int32(dbCfg.MaxConnections)
		if maxConns < 2 {
			maxConns = 2
		}
		poolCfg.MaxConns = maxConns
		poolCfg.MinConns = 1
		poolCfg.MaxConnLifetime = 30 * time.Minute
		poolCfg.MaxConnIdleTime = 5 * time.Minute

		dbPool, err := pgxpool.NewWithConfig(
			context.Background(), poolCfg)
		if err != nil {
			logError("fleet", "db %q: pool: %v", name, err)
			fleetMgr.RegisterInstance(&fleet.DatabaseInstance{
				Name:   name,
				Config: dbCfg,
				Status: &fleet.InstanceStatus{
					Error:    fmt.Sprintf("pool: %v", err),
					LastSeen: time.Now(),
				},
			})
			continue
		}

		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second)
		if err := dbPool.Ping(ctx); err != nil {
			logError("fleet", "db %q: ping: %v", name, err)
			cancel()
			dbPool.Close()
			fleetMgr.RegisterInstance(&fleet.DatabaseInstance{
				Name:   name,
				Config: dbCfg,
				Status: &fleet.InstanceStatus{
					Error:    fmt.Sprintf("ping: %v", err),
					LastSeen: time.Now(),
				},
			})
			continue
		}
		cancel()
		logInfo("fleet", "db %q: connected", name)

		// Bootstrap schema on each database.
		if err := schema.Bootstrap(
			context.Background(), dbPool); err != nil {
			logWarn("fleet", "db %q: schema bootstrap: %v",
				name, err)
		}
		schema.ReleaseAdvisoryLock(context.Background(), dbPool)

		// Config schema migration (adds database_id column).
		if err := schema.MigrateConfigSchema(
			context.Background(), dbPool); err != nil {
			logWarn("fleet",
				"db %q: config migration: %v", name, err)
		}

		// Track first connected pool for sage.databases registration.
		if configPool == nil {
			configPool = dbPool
		}

		// Bootstrap admin user on first database only.
		if i == 0 {
			if err := bootstrapAdminIfEmpty(
				context.Background(), dbPool,
			); err != nil {
				logWarn("fleet", "admin bootstrap: %v", err)
			}
		}

		// Detect PG version for this database.
		var dbPGVersionStr string
		var dbPGVersion int
		_ = dbPool.QueryRow(context.Background(),
			"SHOW server_version_num").Scan(&dbPGVersionStr)
		fmt.Sscanf(dbPGVersionStr, "%d", &dbPGVersion)
		if dbPGVersion == 0 {
			dbPGVersion = 140000 // safe fallback
		}
		logInfo("fleet", "db %q: PG version %d", name, dbPGVersion)

		// Per-database collector.
		dbColl := collector.New(
			dbPool, cfg, dbPGVersion,
			logStructuredWrapper)
		go dbColl.Run(shutdownCtx)

		// Per-database forecaster.
		var fc *forecaster.Forecaster
		if cfg.Forecaster.Enabled {
			fcCfg := forecaster.ForecasterConfig{
				Enabled:             cfg.Forecaster.Enabled,
				LookbackDays:        cfg.Forecaster.LookbackDays,
				DiskWarnGrowthGBDay: cfg.Forecaster.DiskWarnGrowthGBDay,
				ConnectionWarnPct:   cfg.Forecaster.ConnectionWarnPct,
				CacheWarnThreshold:  cfg.Forecaster.CacheWarnThreshold,
			}
			fc = forecaster.New(
				dbPool, fcCfg, logStructuredWrapper)
		}

		// Per-database optimizer.
		var dbOpt *optimizer.Optimizer
		if cfg.LLM.Optimizer.Enabled && llmClient.IsEnabled() {
			optClient := llmClient
			if cfg.LLM.OptimizerLLM.Enabled {
				optClient = llm.NewOptimizerClient(
					&cfg.LLM, &cfg.LLM.OptimizerLLM,
					logStructuredWrapper,
				)
			}
			var fallback *llm.Client
			if cfg.LLM.OptimizerLLM.FallbackToGeneral &&
				optClient != llmClient {
				fallback = llmClient
			}
			dbOpt = optimizer.New(
				optClient, fallback, dbPool,
				&cfg.LLM.Optimizer, dbPGVersion, false,
				cfg.LLM.OptimizerLLM.MaxOutputTokens,
				logStructuredWrapper,
			)
		}

		// Per-database cloud environment detection.
		dbCloudEnv := detectCloudEnv(dbPool)
		logInfo("fleet", "db %q: cloud environment: %s",
			name, dbCloudEnv)

		// Per-database advisor.
		var dbAdvIface analyzer.ConfigAdvisor
		if cfg.Advisor.Enabled && llmClient.IsEnabled() {
			dbAdv := advisor.New(
				dbPool, cfg, dbColl, llmMgr,
				logStructuredWrapper,
			)
			dbAdv.WithCloudEnv(dbCloudEnv)
			dbAdv.WithDatabaseName(dbCfg.Database)
			dbAdvIface = dbAdv
		}

		// Per-database tuner.
		var dbTuner *tuner.Tuner
		if cfg.Tuner.Enabled {
			hpAvail, _ := tuner.DetectHintPlan(
				context.Background(), dbPool)
			tunerCfg := tuner.TunerConfig{
				Enabled:                cfg.Tuner.Enabled,
				LLMEnabled:             cfg.Tuner.LLMEnabled,
				WorkMemMaxMB:           cfg.Tuner.WorkMemMaxMB,
				PlanTimeRatio:          cfg.Tuner.PlanTimeRatio,
				NestedLoopRowThreshold: cfg.Tuner.NestedLoopRowThreshold,
				ParallelMinTableRows:   cfg.Tuner.ParallelMinTableRows,
				MinQueryCalls:          cfg.Tuner.MinQueryCalls,
				VerifyAfterApply:       cfg.Tuner.VerifyAfterApply,
				CascadeCooldownCycles:  cfg.Trust.CascadeCooldownCycles,

				// v0.8.5 Feature 1 — Hint revalidation loop.
				HintRetirementDays:           cfg.Tuner.HintRetirementDays,
				RevalidationIntervalHours:    cfg.Tuner.RevalidationIntervalHours,
				RevalidationKeepRatio:        cfg.Tuner.RevalidationKeepRatio,
				RevalidationRollbackRatio:    cfg.Tuner.RevalidationRollbackRatio,
				RevalidationExplainTimeoutMs: cfg.Tuner.RevalidationExplainTimeoutMs,

				// v0.8.5 Feature 2 — Stale-stats detection + ANALYZE.
				StaleStatsEstimateSkew:        cfg.Tuner.StaleStatsEstimateSkew,
				StaleStatsModRatio:            cfg.Tuner.StaleStatsModRatio,
				StaleStatsAgeMinutes:          cfg.Tuner.StaleStatsAgeMinutes,
				AnalyzeMaxTableMB:             cfg.Tuner.AnalyzeMaxTableMB,
				AnalyzeCooldownMinutes:        cfg.Tuner.AnalyzeCooldownMinutes,
				AnalyzeMaintenanceThresholdMB: cfg.Tuner.AnalyzeMaintenanceThresholdMB,
				AnalyzeTimeoutMs:              cfg.Tuner.AnalyzeTimeoutMs,
				MaxConcurrentAnalyze:          cfg.Tuner.MaxConcurrentAnalyze,
			}
			var tunerOpts []tuner.Option
			if cfg.Tuner.LLMEnabled && llmMgr != nil {
				tc := llmMgr.ForPurpose("query_tuning")
				var fb *llm.Client
				if cfg.LLM.OptimizerLLM.FallbackToGeneral &&
					llmMgr.General != nil {
					fb = llmMgr.General
				}
				tunerOpts = append(tunerOpts,
					tuner.WithLLM(tc, fb))
			}
			dbTuner = tuner.New(dbPool, tunerCfg, hpAvail,
				logStructuredWrapper, tunerOpts...)
			// Per-database hint revalidation loop.
			go dbTuner.StartRevalidationLoop(
				shutdownCtx, tunerCfg.RevalidationIntervalHours)
		}

		// Per-database autoexplain collector.
		if cfg.AutoExplain.Enabled {
			aeAvail, aeErr := autoexplain.Detect(
				context.Background(), dbPool)
			if aeErr != nil {
				logWarn("fleet",
					"db %q auto_explain detect: %v",
					name, aeErr)
			}
			if aeAvail == nil {
				aeAvail = &autoexplain.Availability{}
			}
			aeCfg := autoexplain.CollectorConfig{
				CollectIntervalSeconds: cfg.AutoExplain.CollectIntervalSeconds,
				MaxPlansPerCycle:       cfg.AutoExplain.MaxPlansPerCycle,
				LogMinDurationMs:       cfg.AutoExplain.LogMinDurationMs,
				PreferSessionLoad:      cfg.AutoExplain.PreferSessionLoad,
			}
			aec := autoexplain.NewCollector(
				dbPool, aeCfg, aeAvail,
				logStructuredWrapper,
			)
			go aec.Run(shutdownCtx)
		}

		// Per-database briefing worker.
		var dbBrief *briefing.Worker
		if llmClient.IsEnabled() {
			dbBrief = briefing.New(
				dbPool, cfg, llmClient,
				logStructuredWrapper,
			)
		}

		dbAnal := analyzer.New(
			dbPool, cfg, dbColl, dbOpt, dbAdvIface, fc, dbTuner,
			logStructuredWrapper)
		go dbAnal.Run(shutdownCtx)

		// Per-database executor. Pass the YAML-parsed configRampStart
		// so the first-time bootstrap of each database's sage.config
		// row honours the operator's configured ramp start rather than
		// silently defaulting to now().
		rStart, _ := schema.PersistTrustRampStart(
			context.Background(), dbPool, configRampStart)
		dbExec := executor.New(
			dbPool, cfg, dbAnal, rStart,
			logStructuredWrapper)
		dbExec.WithAnalyzeSemaphore(analyzeSem)
		dbActionStore := store.NewActionStore(dbPool)
		dbExec.WithActionStore(dbActionStore, "auto")

		// Notification dispatcher per database.
		dbDispatcher := notify.NewDispatcher(
			dbPool, logStructuredWrapper)
		dbExec.WithDispatcher(dbDispatcher)
		dbExec.WithDatabaseName(name)
		dbAnal.WithDispatcher(dbDispatcher)
		dbAnal.WithDatabaseName(name)

		go store.StartActionExpiry(
			shutdownCtx, dbActionStore, logStructuredWrapper)

		inst := &fleet.DatabaseInstance{
			Name:      name,
			Config:    dbCfg,
			Pool:      dbPool,
			Collector: dbColl,
			Analyzer:  dbAnal,
			Executor:  dbExec,
			Status: &fleet.InstanceStatus{
				Connected:    true,
				TrustLevel:   dbCfg.TrustLevel,
				DatabaseName: name,
				LastSeen:     time.Now(),
			},
		}
		fleetMgr.RegisterInstance(inst)
		// Populate findings immediately so API doesn't show zeros
		// during the first ticker interval.
		updateInstanceFindings(shutdownCtx, inst)

		// Per-database orchestrator.
		go fleetDBOrchestrator(
			name, dbPool, dbExec, dbBrief, dbCfg)

		features := "collector+analyzer+executor"
		if dbOpt != nil {
			features += "+optimizer"
		}
		if dbAdvIface != nil {
			features += "+advisor"
		}
		if dbTuner != nil {
			features += "+tuner"
		}
		if dbBrief != nil {
			features += "+briefing"
		}
		logInfo("fleet", "db %q: initialized (%s)", name, features)
	}

	// Register fleet databases in sage.databases for config API.
	if configPool != nil {
		registerFleetDatabases(configPool)
	}

	logInfo("fleet", "%d databases initialized",
		len(cfg.Databases))
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

// fleetDBOrchestrator runs executor, briefing, and retention
// cycles for a single fleet database.
func fleetDBOrchestrator(
	name string,
	dbPool *pgxpool.Pool,
	dbExec *executor.Executor,
	dbBrief *briefing.Worker,
	dbCfg config.DatabaseConfig,
) {
	interval := cfg.Analyzer.Interval() + 5*time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if shutdownFlag {
				return
			}
			if dbExec != nil {
				dbExec.RunCycle(shutdownCtx, false)
			}
			// Briefing (scheduled).
			if dbBrief != nil &&
				dbBrief.ShouldRun(time.Now()) {
				text, bErr := dbBrief.Generate(shutdownCtx)
				if bErr != nil {
					logWarn("briefing",
						"[%s] generation failed: %v",
						name, bErr)
				} else {
					dbBrief.Dispatch(text)
					dbBrief.MarkRan()
				}
			}
			// Update fleet status for this instance.
			if inst := fleetMgr.GetInstance(name); inst != nil {
				updateInstanceFindings(shutdownCtx, inst)
			}
		case <-shutdownCtx.Done():
			return
		}
	}
}

// buildFleetLLMFeatures creates per-database optimizer, advisor,
// tuner, and briefing worker using the shared LLM client.
func buildFleetLLMFeatures(
	dbPool *pgxpool.Pool,
	dbPGVersion int,
	dbColl *collector.Collector,
	databaseName string,
) (
	*optimizer.Optimizer,
	analyzer.ConfigAdvisor,
	*tuner.Tuner,
	*briefing.Worker,
) {
	if llmClient == nil || !llmClient.IsEnabled() {
		return nil, nil, nil, nil
	}

	// Optimizer.
	var dbOpt *optimizer.Optimizer
	if cfg.LLM.Optimizer.Enabled {
		optClient := llmClient
		if cfg.LLM.OptimizerLLM.Enabled {
			optClient = llm.NewOptimizerClient(
				&cfg.LLM, &cfg.LLM.OptimizerLLM,
				logStructuredWrapper,
			)
		}
		var fallback *llm.Client
		if cfg.LLM.OptimizerLLM.FallbackToGeneral &&
			optClient != llmClient {
			fallback = llmClient
		}
		dbOpt = optimizer.New(
			optClient, fallback, dbPool,
			&cfg.LLM.Optimizer, dbPGVersion, false,
			cfg.LLM.OptimizerLLM.MaxOutputTokens,
			logStructuredWrapper,
		)
	}

	// Advisor (with per-database cloud detection).
	var dbAdvIface analyzer.ConfigAdvisor
	if cfg.Advisor.Enabled {
		dbAdv := advisor.New(
			dbPool, cfg, dbColl, llmMgr,
			logStructuredWrapper,
		)
		dbCloudEnv := detectCloudEnv(dbPool)
		dbAdv.WithCloudEnv(dbCloudEnv)
		dbAdv.WithDatabaseName(databaseName)
		dbAdvIface = dbAdv
	}

	// Tuner.
	var dbTuner *tuner.Tuner
	if cfg.Tuner.Enabled {
		hpAvail, _ := tuner.DetectHintPlan(
			context.Background(), dbPool)
		tunerCfg := tuner.TunerConfig{
			Enabled:                cfg.Tuner.Enabled,
			LLMEnabled:             cfg.Tuner.LLMEnabled,
			WorkMemMaxMB:           cfg.Tuner.WorkMemMaxMB,
			PlanTimeRatio:          cfg.Tuner.PlanTimeRatio,
			NestedLoopRowThreshold: cfg.Tuner.NestedLoopRowThreshold,
			ParallelMinTableRows:   cfg.Tuner.ParallelMinTableRows,
			MinQueryCalls:          cfg.Tuner.MinQueryCalls,
			VerifyAfterApply:       cfg.Tuner.VerifyAfterApply,
			CascadeCooldownCycles:  cfg.Trust.CascadeCooldownCycles,

			// v0.8.5 Feature 1 — Hint revalidation loop.
			HintRetirementDays:           cfg.Tuner.HintRetirementDays,
			RevalidationIntervalHours:    cfg.Tuner.RevalidationIntervalHours,
			RevalidationKeepRatio:        cfg.Tuner.RevalidationKeepRatio,
			RevalidationRollbackRatio:    cfg.Tuner.RevalidationRollbackRatio,
			RevalidationExplainTimeoutMs: cfg.Tuner.RevalidationExplainTimeoutMs,

			// v0.8.5 Feature 2 — Stale-stats detection + ANALYZE.
			StaleStatsEstimateSkew:        cfg.Tuner.StaleStatsEstimateSkew,
			StaleStatsModRatio:            cfg.Tuner.StaleStatsModRatio,
			StaleStatsAgeMinutes:          cfg.Tuner.StaleStatsAgeMinutes,
			AnalyzeMaxTableMB:             cfg.Tuner.AnalyzeMaxTableMB,
			AnalyzeCooldownMinutes:        cfg.Tuner.AnalyzeCooldownMinutes,
			AnalyzeMaintenanceThresholdMB: cfg.Tuner.AnalyzeMaintenanceThresholdMB,
			AnalyzeTimeoutMs:              cfg.Tuner.AnalyzeTimeoutMs,
			MaxConcurrentAnalyze:          cfg.Tuner.MaxConcurrentAnalyze,
		}
		var tunerOpts []tuner.Option
		if cfg.Tuner.LLMEnabled && llmMgr != nil {
			tc := llmMgr.ForPurpose("query_tuning")
			var fb *llm.Client
			if cfg.LLM.OptimizerLLM.FallbackToGeneral &&
				llmMgr.General != nil {
				fb = llmMgr.General
			}
			tunerOpts = append(tunerOpts,
				tuner.WithLLM(tc, fb))
		}
		dbTuner = tuner.New(dbPool, tunerCfg, hpAvail,
			logStructuredWrapper, tunerOpts...)
	}

	// Briefing.
	dbBrief := briefing.New(
		dbPool, cfg, llmClient, logStructuredWrapper,
	)

	return dbOpt, dbAdvIface, dbTuner, dbBrief
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
		return "auto"
	}
	return "auto"
}

func pgVersionString(num int) string {
	if num == 0 {
		return "unknown"
	}
	major := num / 10000
	minor := num % 100
	return fmt.Sprintf("%d.%d", major, minor)
}

func startAPIServer(rl *RateLimiter) {
	addr := cfg.API.ListenAddr
	if addr == "" {
		addr = ":8080"
	}

	result := wireRouter(WireParams{
		Cfg:         cfg,
		Pool:        pool,
		FleetMgr:    fleetMgr,
		LLMMgr:      llmMgr,
		MetaState:   globalMetaState,
		Actions: struct {
			Store    *store.ActionStore
			Executor *executor.Executor
		}{
			Store:    actionStore,
			Executor: exec,
		},
		RateLimiter: rl,
	})

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

func updateFleetStatus(ctx context.Context) {
	instances := fleetMgr.Instances()
	for _, inst := range instances {
		updateInstanceFindings(ctx, inst)
	}
}

func updateInstanceFindings(
	ctx context.Context,
	inst *fleet.DatabaseInstance,
) {
	dbPool := inst.Pool
	if dbPool == nil {
		dbPool = pool // fallback to global for standalone
	}
	if dbPool == nil {
		return
	}
	rows, err := dbPool.Query(ctx,
		`SELECT severity, count(*)
		   FROM sage.findings
		  WHERE status = 'open'
		  GROUP BY severity`)
	if err != nil {
		logWarn("fleet", "findings query: %v", err)
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
	inst.Status.FindingsOpen = open
	inst.Status.FindingsCritical = critical
	inst.Status.FindingsWarning = warning
	inst.Status.FindingsInfo = info
	inst.Status.AnalyzerLastRun = time.Now()
	inst.Status.LastSeen = time.Now()
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
	if extensionAvailable {
		var ver string
		if err := pool.QueryRow(ctx, "SELECT sage.status()->>'version'").Scan(&ver); err != nil {
			ver = "unknown"
		}
		fmt.Fprintf(&b, "pg_sage_info{version=%q,mode=\"extension\"} 1\n\n", ver)
	} else {
		fmt.Fprintf(&b, "pg_sage_info{version=%q,mode=%q} 1\n\n", version, cfg.Mode)
	}

	// Mode metric.
	b.WriteString("# HELP pg_sage_mode Operating mode (0=extension, 1=standalone)\n# TYPE pg_sage_mode gauge\n")
	modeVal := 0
	if cfg.IsStandalone() {
		modeVal = 1
	}
	fmt.Fprintf(&b, "pg_sage_mode %d\n\n", modeVal)

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

	if extensionAvailable {
		writeExtensionMetrics(&b, ctx)
	}

	// Standalone metrics.
	if cfg.IsStandalone() {
		writeStandaloneMetrics(&b, ctx)
	}

	// Fleet metrics.
	if cfg.Mode == "fleet" && fleetMgr != nil {
		writeFleetMetrics(&b)
	}

	// Database metrics (only when global pool exists).
	if pool != nil {
		writeDatabaseMetrics(&b, ctx)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, b.String())
}

func writeExtensionMetrics(b *strings.Builder, ctx context.Context) {
	// Findings.
	b.WriteString("# HELP pg_sage_findings_total Open findings by severity\n# TYPE pg_sage_findings_total gauge\n")
	rows, err := pool.Query(ctx, `SELECT severity, count(*) FROM sage.findings WHERE status = 'open' GROUP BY severity`)
	if err == nil {
		defer rows.Close()
		found := map[string]int64{}
		for rows.Next() {
			var sev string
			var cnt int64
			if rows.Scan(&sev, &cnt) == nil {
				found[sev] = cnt
			}
		}
		for _, sev := range []string{"critical", "warning", "info"} {
			fmt.Fprintf(b, "pg_sage_findings_total{severity=%q} %d\n", sev, found[sev])
		}
		b.WriteString("\n")
	}

	// Circuit breaker.
	b.WriteString("# HELP pg_sage_circuit_breaker_state Circuit breaker (0=closed, 1=open)\n# TYPE pg_sage_circuit_breaker_state gauge\n")
	var statusJSON string
	if err := pool.QueryRow(ctx, "SELECT sage.status()::text").Scan(&statusJSON); err == nil {
		var status map[string]any
		if json.Unmarshal([]byte(statusJSON), &status) == nil {
			dbState, llmState := 0, 0
			if v, ok := status["circuit_state"].(string); ok && v != "closed" {
				dbState = 1
			}
			if v, ok := status["llm_circuit_state"].(string); ok && v != "closed" {
				llmState = 1
			}
			fmt.Fprintf(b, "pg_sage_circuit_breaker_state{breaker=\"db\"} %d\n", dbState)
			fmt.Fprintf(b, "pg_sage_circuit_breaker_state{breaker=\"llm\"} %d\n", llmState)
		}
	}
	b.WriteString("\n")
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
}

func NewRateLimiter(maxPerMinute int) *RateLimiter {
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
	close(rl.stop)
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
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-rl.interval)
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
			rl.mu.Unlock()
		case <-rl.stop:
			return
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

var trustedProxies = map[string]bool{
	"127.0.0.1": true,
	"::1":       true,
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	// Only trust X-Forwarded-For from known proxies
	if trustedProxies[host] {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}

	return host
}


// --- Detection ---

func detectExtension() bool {
	if pool == nil {
		return false // fleet mode: no global pool
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'sage')
		AND EXISTS (
			SELECT 1 FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'sage' AND p.proname = 'health_json'
		)
	`).Scan(&exists)
	if err != nil {
		return false
	}
	return exists
}

func detectCloudEnvironment() string {
	return detectCloudEnv(pool)
}

// detectCloudEnv probes a connection pool for managed service
// indicators. Pass any per-database pool in fleet mode.
func detectCloudEnv(p *pgxpool.Pool) string {
	if p == nil {
		return "unknown"
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
	logStructured("INFO", component, msg, args...)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
