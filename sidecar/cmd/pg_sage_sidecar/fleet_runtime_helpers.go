package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
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

func newFleetOptimizerClient(
	databaseName string, general *llm.Client,
) *llm.Client {
	if !cfg.LLM.OptimizerLLM.Enabled {
		return general
	}
	client := llm.NewOptimizerClient(
		&cfg.LLM, &cfg.LLM.OptimizerLLM, logStructuredWrapper,
	)
	if fleetLLMBudget != nil {
		client.SetBudget(dbBudget{b: fleetLLMBudget, db: databaseName})
	}
	return client
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
		pgVersion, false, cfg.LLM.OptimizerLLM.MaxOutputTokens,
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
