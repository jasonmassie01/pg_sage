package main

import (
	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/forecaster"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// startMonitoring builds and starts the collector and analyzer with every
// optional analysis feature, then the notification and alerting paths.
// The analyzer is fully configured before its goroutine starts (G2-B15).
func (rt *databaseRuntime) startMonitoring() {
	rt.collector = rt.newCollector()
	rt.start(func() { rt.collector.Run(rt.ctx) })
	rt.note("collector")
	explain := rt.startAutoExplain()
	// Interfaces stay true nils when a feature is off (no typed nils).
	var queryTuner analyzer.QueryTuner
	var tuningAgent analyzer.TuningProducer
	qt := rt.newTuner()
	if qt != nil {
		queryTuner = qt
	}
	if agent := rt.newTuningAgent(explain, qt); agent != nil {
		tuningAgent = agent
	}
	rt.analyzer = analyzer.New(
		rt.spec.Pool, cfg, rt.collector, tuningAgent,
		rt.newAdvisor(), rt.newForecaster(), queryTuner,
		logStructuredWrapper,
	)
	rt.analyzer.WithFactFilter(rt.factFilter())
	rt.analyzer.WithSupplementalDetector(executor.NewRunawayDetector(
		rt.spec.Pool, &cfg.Runaway, logStructuredWrapper,
	))
	rt.note("analyzer")
	rt.wireRCA()
	rt.startInvestigator()
	if rt.dispatcher != nil {
		rt.analyzer.WithDispatcher(rt.dispatcher)
	}
	rt.analyzer.WithDatabaseName(rt.spec.Name)
	rt.analyzer.WithPolicyVersion(rt.policyVersionReader())
	if rt.llmOn {
		rt.analyzer.WithPlanNarrator(
			analyzer.NewLLMPlanNarrator(rt.generalLLM, logStructuredWrapper))
	}
	rt.start(func() { rt.analyzer.Run(rt.ctx) })
	if alerts := newInstanceAlertManager(rt.spec.Pool); alerts != nil {
		rt.start(func() { alerts.Run(rt.ctx) })
		rt.note("alerting")
	}
	rt.startSchemaLint()
	rt.startMigrationAdvisor()
}

// newCollector builds the stats collector. The configuration snapshot is
// skipped when neither the advisor nor the tuning agent will consume it:
// both default on, but run only with a usable LLM.
func (rt *databaseRuntime) newCollector() *collector.Collector {
	result := collector.New(
		rt.spec.Pool, rt.cfg, rt.pgVersion(), logStructuredWrapper,
	)
	if !rt.advisorActive() && !rt.tuningActive() {
		result.WithoutConfigSnapshots()
	}
	return result
}

// advisorActive reports whether this runtime builds the config advisor:
// advisor.enabled and an LLM that was usable when the runtime started.
func (rt *databaseRuntime) advisorActive() bool {
	return cfg.Advisor.Enabled && rt.llmOn
}

// startAutoExplain starts the plan collector when auto_explain is enabled.
// It runs even without the extension: plain EXPLAIN still captures plans
// for non-parameterized queries. It reports whether the extension is there.
func (rt *databaseRuntime) startAutoExplain() bool {
	if !cfg.AutoExplain.Enabled {
		return false
	}
	avail, err := autoexplain.Detect(rt.ctx, rt.spec.Pool)
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: auto_explain detection: %v", rt.spec.Name, err)
	}
	if avail == nil {
		avail = &autoexplain.Availability{}
	}
	if !avail.Available {
		if hint := autoexplain.EnableHint(rt.provider); hint != "" {
			logWarn(rt.spec.Scope, "db %q: auto_explain not available; to enable "+
				"on %s: %s", rt.spec.Name, rt.provider, hint)
		}
	}
	collectorCfg := autoexplain.CollectorConfig{
		CollectIntervalSeconds: cfg.AutoExplain.CollectIntervalSeconds,
		MaxPlansPerCycle:       cfg.AutoExplain.MaxPlansPerCycle,
		LogMinDurationMs:       cfg.AutoExplain.LogMinDurationMs,
		PreferSessionLoad:      cfg.AutoExplain.PreferSessionLoad,
	}
	plans := autoexplain.NewCollector(
		rt.spec.Pool, collectorCfg, avail, logStructuredWrapper,
	)
	rt.start(func() { plans.Run(rt.ctx) })
	rt.note("auto_explain")
	return avail.Available
}

// newAdvisor builds the config advisor. It targets the PostgreSQL database
// name, not the instance name, when it rewrites settings for the provider.
func (rt *databaseRuntime) newAdvisor() analyzer.ConfigAdvisor {
	if !rt.advisorActive() {
		return nil
	}
	result := advisor.New(
		rt.spec.Pool, cfg, rt.collector, rt.llmManager, logStructuredWrapper,
	)
	result.WithCloudEnv(rt.provider)
	result.WithDatabaseName(rt.spec.Config.Database)
	result.WithFacts(rt.facts)
	rt.note("advisor")
	return result
}

func (rt *databaseRuntime) newForecaster() analyzer.WorkloadForecaster {
	if !cfg.Forecaster.Enabled {
		return nil
	}
	rt.note("forecaster")
	return forecaster.New(rt.spec.Pool, forecasterConfig(cfg), logStructuredWrapper)
}

func forecasterConfig(c *config.Config) forecaster.ForecasterConfig {
	return forecaster.ForecasterConfig{
		Enabled:              c.Forecaster.Enabled,
		LookbackDays:         c.Forecaster.LookbackDays,
		DiskWarnGrowthGBDay:  c.Forecaster.DiskWarnGrowthGBDay,
		ConnectionWarnPct:    c.Forecaster.ConnectionWarnPct,
		CacheWarnThreshold:   c.Forecaster.CacheWarnThreshold,
		SequenceWarnDays:     c.Forecaster.SequenceWarnDays,
		SequenceCriticalDays: c.Forecaster.SequenceCriticalDays,
		MinDataPoints:        c.Forecaster.MinDataPoints,
		AlertHorizons:        c.Forecaster.AlertHorizons,
		DiskCapacityBytes:    c.Forecaster.DiskCapacityBytes,
		MinRSquared:          c.Forecaster.MinRSquared,
	}
}

// newTuner builds the query tuner, which runs its deterministic rules and
// validates, clamps and records the tuning agent's hints, and starts its
// hint revalidation loop.
func (rt *databaseRuntime) newTuner() *tuner.Tuner {
	if !cfg.Tuner.Enabled {
		return nil
	}
	hintPlan, err := tuner.DetectHintPlan(rt.ctx, rt.spec.Pool)
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: pg_hint_plan detection: %v", rt.spec.Name, err)
	}
	tunerCfg := tunerConfig(cfg)
	result := tuner.New(rt.spec.Pool, tunerCfg, hintPlan, logStructuredWrapper)
	rt.start(func() {
		result.StartRevalidationLoop(rt.ctx, tunerCfg.RevalidationIntervalHours)
	})
	rt.note("tuner")
	return result
}

func tunerConfig(c *config.Config) tuner.TunerConfig {
	return tuner.TunerConfig{
		Enabled:                      c.Tuner.Enabled,
		WorkMemMaxMB:                 c.Tuner.WorkMemMaxMB,
		PlanTimeRatio:                c.Tuner.PlanTimeRatio,
		NestedLoopRowThreshold:       c.Tuner.NestedLoopRowThreshold,
		ParallelMinTableRows:         c.Tuner.ParallelMinTableRows,
		MinQueryCalls:                c.Tuner.MinQueryCalls,
		VerifyAfterApply:             c.Tuner.VerifyAfterApply,
		CascadeCooldownCycles:        c.Trust.CascadeCooldownCycles,
		HintRetirementDays:           c.Tuner.HintRetirementDays,
		RevalidationIntervalHours:    c.Tuner.RevalidationIntervalHours,
		RevalidationKeepRatio:        c.Tuner.RevalidationKeepRatio,
		RevalidationRollbackRatio:    c.Tuner.RevalidationRollbackRatio,
		RevalidationExplainTimeoutMs: c.Tuner.RevalidationExplainTimeoutMs,
		StaleStatsEstimateSkew:       c.Tuner.StaleStatsEstimateSkew,
		StaleStatsModRatio:           c.Tuner.StaleStatsModRatio,
		StaleStatsAgeMinutes:         c.Tuner.StaleStatsAgeMinutes,
		AnalyzeMaxTableMB:            c.Tuner.AnalyzeMaxTableMB,
		AnalyzeCooldownMinutes:       c.Tuner.AnalyzeCooldownMinutes,
		AnalyzeTimeoutMs:             c.Tuner.AnalyzeTimeoutMs,
		MaxConcurrentAnalyze:         c.Tuner.MaxConcurrentAnalyze,
	}
}
