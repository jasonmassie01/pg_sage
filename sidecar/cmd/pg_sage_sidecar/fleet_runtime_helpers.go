package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/startup"
	"github.com/pg-sage/sidecar/internal/tuner"
)

func initializeAnalyzeSemaphore() {
	maxAnalyze := cfg.Tuner.MaxConcurrentAnalyze
	if maxAnalyze <= 0 {
		maxAnalyze = 1
	}
	analyzeSem = make(chan struct{}, maxAnalyze)
	logInfo("startup", "ANALYZE semaphore sized to %d concurrent slots",
		maxAnalyze)
}

// newFleetDBLLMClients builds the registry-tracked general and optimizer
// clients for one database. allowed=false (databases[].llm_enabled: false)
// yields clients that stay disabled across every reconfigure (G5-B07).
func newFleetDBLLMClients(
	databaseName string, allowed bool,
) (*llm.Client, *llm.Manager) {
	general := llmClients.newClient(llmRoleGeneral, databaseName, allowed)
	attachFleetBudget(general, databaseName)
	optimizerClient := general
	if cfg.LLM.OptimizerLLM.Enabled {
		optimizerClient = llmClients.newClient(
			llmRoleOptimizer, databaseName, allowed,
		)
		attachFleetBudget(optimizerClient, databaseName)
	}
	return general, llm.NewManager(
		general, optimizerClient, cfg.LLM.OptimizerLLM.FallbackToGeneral,
	)
}

// attachFleetBudget registers the database with the fleet budget (so
// databases added at runtime get a share, G5-B06) and scopes the client.
func attachFleetBudget(client *llm.Client, databaseName string) {
	if fleetLLMBudget == nil {
		return
	}
	fleetLLMBudget.Register(databaseName)
	client.SetBudget(dbBudget{b: fleetLLMBudget, db: databaseName})
}

func newFleetOptimizer(
	pool *pgxpool.Pool,
	pgVersion int,
	general, optimizerClient *llm.Client,
) *optimizer.Optimizer {
	if !cfg.LLM.Optimizer.Enabled {
		return nil
	}
	var fallback *llm.Client
	if cfg.LLM.OptimizerLLM.FallbackToGeneral &&
		optimizerClient != general {
		fallback = general
	}
	return optimizer.New(
		optimizerClient, fallback, pool, &cfg.LLM.Optimizer,
		pgVersion, cfg.LLM.OptimizerLLM.MaxOutputTokens,
		logStructuredWrapper,
	)
}

func newFleetAdvisor(
	pool *pgxpool.Pool,
	coll *collector.Collector,
	databaseName string,
	manager *llm.Manager,
) analyzer.ConfigAdvisor {
	if !cfg.Advisor.Enabled {
		return nil
	}
	result := advisor.New(pool, cfg, coll, manager, logStructuredWrapper)
	result.WithCloudEnv(detectCloudEnv(pool))
	result.WithDatabaseName(databaseName)
	return result
}

func newFleetTuner(
	pool *pgxpool.Pool, manager *llm.Manager,
) *tuner.Tuner {
	if !cfg.Tuner.Enabled {
		return nil
	}
	hintPlan, _ := tuner.DetectHintPlan(context.Background(), pool)
	options := fleetTunerOptions(manager)
	return tuner.New(
		pool, fleetTunerConfig(), hintPlan,
		logStructuredWrapper, options...,
	)
}

func fleetTunerOptions(manager *llm.Manager) []tuner.Option {
	if !cfg.Tuner.LLMEnabled || manager == nil {
		return nil
	}
	primary := manager.ForPurpose("query_tuning")
	var fallback *llm.Client
	if cfg.LLM.OptimizerLLM.FallbackToGeneral {
		fallback = manager.General
	}
	return []tuner.Option{tuner.WithLLM(primary, fallback)}
}

func fleetTunerConfig() tuner.TunerConfig {
	return tuner.TunerConfig{
		Enabled:                       cfg.Tuner.Enabled,
		LLMEnabled:                    cfg.Tuner.LLMEnabled,
		WorkMemMaxMB:                  cfg.Tuner.WorkMemMaxMB,
		PlanTimeRatio:                 cfg.Tuner.PlanTimeRatio,
		NestedLoopRowThreshold:        cfg.Tuner.NestedLoopRowThreshold,
		ParallelMinTableRows:          cfg.Tuner.ParallelMinTableRows,
		MinQueryCalls:                 cfg.Tuner.MinQueryCalls,
		VerifyAfterApply:              cfg.Tuner.VerifyAfterApply,
		CascadeCooldownCycles:         cfg.Trust.CascadeCooldownCycles,
		HintRetirementDays:            cfg.Tuner.HintRetirementDays,
		RevalidationIntervalHours:     cfg.Tuner.RevalidationIntervalHours,
		RevalidationKeepRatio:         cfg.Tuner.RevalidationKeepRatio,
		RevalidationRollbackRatio:     cfg.Tuner.RevalidationRollbackRatio,
		RevalidationExplainTimeoutMs:  cfg.Tuner.RevalidationExplainTimeoutMs,
		StaleStatsEstimateSkew:        cfg.Tuner.StaleStatsEstimateSkew,
		StaleStatsModRatio:            cfg.Tuner.StaleStatsModRatio,
		StaleStatsAgeMinutes:          cfg.Tuner.StaleStatsAgeMinutes,
		AnalyzeMaxTableMB:             cfg.Tuner.AnalyzeMaxTableMB,
		AnalyzeCooldownMinutes:        cfg.Tuner.AnalyzeCooldownMinutes,
		AnalyzeMaintenanceThresholdMB: cfg.Tuner.AnalyzeMaintenanceThresholdMB,
		AnalyzeTimeoutMs:              cfg.Tuner.AnalyzeTimeoutMs,
		MaxConcurrentAnalyze:          cfg.Tuner.MaxConcurrentAnalyze,
	}
}

func initializeFleetBudget(databaseNames []string) {
	fleetLLMBudget = nil
	if cfg.LLM.FleetTokenBudgetDaily <= 0 {
		return
	}
	fleetLLMBudget = fleet.NewBudget(
		cfg.LLM.FleetTokenBudgetDaily, databaseNames,
	)
	go resetFleetBudgetDaily(shutdownCtx, fleetLLMBudget)
	logInfo("fleet", "per-database LLM budget enabled: "+
		"%d tokens/day across %d databases",
		cfg.LLM.FleetTokenBudgetDaily, len(databaseNames))
}

// runInstanceChecks validates one monitored database's prerequisites. A
// variable so tests can stub it.
var runInstanceChecks = startup.RunChecks

// detectInstanceVersion probes server_version_num; a variable for tests.
var detectInstanceVersion = detectPGVersion

// instanceChecksOrDegraded runs the per-database prerequisite checks. A
// failure (for example pg_stat_statements not installed) keeps the
// database monitored in degraded mode — capability flags off — and is
// returned as a warning. Dropping the database instead also skipped the
// first-admin bootstrap and locked the dashboard.
func instanceChecksOrDegraded(
	ctx context.Context, pool *pgxpool.Pool,
) (*startup.CheckResult, error) {
	checks, err := runInstanceChecks(ctx, pool)
	if err == nil && checks != nil {
		return checks, nil
	}
	if err == nil {
		err = errors.New("prerequisite checks returned no result")
	}
	return &startup.CheckResult{PGVersionNum: detectInstanceVersion(pool)}, err
}

// instanceRuntimeConfig clones the global config and carries the database's
// own capability flags, so fleet and meta collectors select WAL and
// plan-time columns per instance (G5-B13, G1-B07).
func instanceRuntimeConfig(checks *startup.CheckResult) *config.Config {
	runtimeCfg := config.Clone(cfg)
	runtimeCfg.PGVersionNum = checks.PGVersionNum
	runtimeCfg.HasWALColumns = checks.HasWALColumns
	runtimeCfg.HasPlanTimeColumns = checks.HasPlanTimeColumns
	return runtimeCfg
}
