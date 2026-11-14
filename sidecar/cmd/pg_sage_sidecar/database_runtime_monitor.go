package main

import (
	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/forecaster"
	"github.com/pg-sage/sidecar/internal/tuner"
	"github.com/pg-sage/sidecar/internal/tuning"
	"github.com/pg-sage/sidecar/internal/verify"
)

// startMonitoring builds and starts the collector and analyzer with every
// optional analysis feature, then the notification and alerting paths.
// The analyzer is fully configured before its goroutine starts (G2-B15).
func (rt *databaseRuntime) startMonitoring() {
	rt.collector = rt.newCollector()
	rt.start(func() { rt.collector.Run(rt.ctx) })
	rt.note("collector")
	rt.startAutoExplain()
	rt.joinInstanceOwner()
	// Interfaces stay true nils when a feature is off (no typed nils).
	var queryTuner analyzer.QueryTuner
	var tuningAgent analyzer.TuningProducer
	qt := rt.newTuner()
	if qt != nil {
		queryTuner = qt
	}
	if agent := rt.newTuningAgent(qt); agent != nil {
		tuningAgent = agent
	}
	rt.analyzer = analyzer.New(
		rt.spec.Pool, cfg, rt.collector, tuningAgent,
		rt.newAdvisor(), rt.newForecaster(), queryTuner,
		logStructuredWrapper,
	)
	rt.attachAnalyzerInputs()
	rt.attachSupplementalDetectors()
	rt.note("analyzer")
	rt.wireRCA()
	rt.startInvestigator()
	if rt.dispatcher != nil {
		rt.analyzer.WithDispatcher(rt.dispatcher)
	}
	rt.analyzer.WithDatabaseName(rt.spec.Name)
	// Archiving is cluster-wide: the instance's owner reports it (market-07).
	rt.analyzer.WithInstanceOwner(rt.instanceOwnerFunc())
	rt.analyzer.WithPolicyVersion(rt.policyVersionReader())
	if rt.llmOn {
		rt.analyzer.WithPlanNarrator(
			analyzer.NewLLMPlanNarrator(rt.generalLLM, logStructuredWrapper))
	}
	rt.start(func() { rt.analyzer.Run(rt.ctx) })
	if alerts := newInstanceAlertManager(rt.spec.Pool, rt.spec.Name); alerts != nil {
		rt.start(func() { alerts.Run(rt.ctx) })
		rt.note("alerting")
	} else {
		rt.startLegacyCutover()
	}
	rt.startSchemaLint()
	rt.startMigrationAdvisor()
	rt.startPGAuditCorrelation()
	rt.startObservedPlans()
}

// attachSupplementalDetectors adds the analyzer's extra per-cycle detectors:
// runaway queries, agent posture, workload budgets (market-08) and upgrade
// readiness (market-05).
func (rt *databaseRuntime) attachSupplementalDetectors() {
	rt.analyzer.WithSupplementalDetector(executor.NewRunawayDetector(
		rt.spec.Pool, &cfg.Runaway, logStructuredWrapper,
	))
	rt.analyzer.WithSupplementalDetector(rt.newPostureMonitor())
	rt.analyzer.WithSupplementalDetector(rt.newBudgetMonitor())
	rt.analyzer.WithSupplementalDetector(rt.newUpgradeMonitor())
}

// attachAnalyzerInputs gives the analyzer the index floor (when on), the
// fact filter, the freeze sampler and its rule packs.
func (rt *databaseRuntime) attachAnalyzerInputs() {
	if floor := rt.newIndexFloor(); floor != nil {
		rt.analyzer.WithIndexFloor(floor)
	}
	rt.analyzer.WithFactFilter(rt.factFilter())
	rt.analyzer.WithFreezeHorizons(rt.freezeCustodian().Sampler())
	rt.attachRulePacks()
}

// newCollector builds the stats collector. The configuration snapshot is
// skipped when neither the advisor nor the tuning agent will consume it:
// both default on, but run only with a usable LLM. The open index
// verifications' targets are pinned (collection-07): sampled whatever
// their rank, so an index that made its query cheap is still measured.
func (rt *databaseRuntime) newCollector() *collector.Collector {
	result := collector.New(
		rt.spec.Pool, rt.cfg, rt.pgVersion(), logStructuredWrapper,
	)
	verifications := verify.NewPostgresStateStore(rt.spec.Pool, 1)
	result.WithPinSource(collector.PinSourceFunc(verifications.OpenTargetIDs))
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
// for non-parameterized queries.
func (rt *databaseRuntime) startAutoExplain() {
	if !cfg.AutoExplain.Enabled {
		return
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
	}
	plans := autoexplain.NewCollector(
		rt.spec.Pool, collectorCfg, avail, logStructuredWrapper,
	)
	rt.start(func() { plans.Run(rt.ctx) })
	rt.note("auto_explain")
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
	result.WithPGVersion(rt.pgVersion())
	result.WithHostMemorySource(rt.hostMemory)
	result.WithDatabaseName(rt.spec.Config.Database)
	result.WithFacts(rt.facts)
	result.WithInstanceOwner(rt.instanceOwnerFunc())
	rt.note("advisor")
	return result
}

func (rt *databaseRuntime) newForecaster() analyzer.WorkloadForecaster {
	if !cfg.Forecaster.Enabled {
		return nil
	}
	rt.note("forecaster")
	return forecaster.New(rt.spec.Pool, forecasterConfig(cfg), logStructuredWrapper).
		WithCapacity(rt.runwayCapacity()).
		WithClusterOwner(rt.forecastClusterOwner())
}

func forecasterConfig(c *config.Config) forecaster.ForecasterConfig {
	return forecaster.ForecasterConfig{
		Enabled:                    c.Forecaster.Enabled,
		LookbackDays:               c.Forecaster.LookbackDays,
		DiskWarnGrowthGBDay:        c.Forecaster.DiskWarnGrowthGBDay,
		ConnectionWarnPct:          c.Forecaster.ConnectionWarnPct,
		CacheWarnThreshold:         c.Forecaster.CacheWarnThreshold,
		CacheMinBlocksPerDay:       c.Forecaster.CacheMinBlocksPerDay,
		CheckpointRequestedPerHour: c.Forecaster.CheckpointRequestedPerHour,
		DiskCapacityBytes:          c.Forecaster.DiskCapacityBytes,
		MinRSquared:                c.Forecaster.MinRSquared,
		CollectorInterval:          c.Collector.Interval(),
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
	if !hintPlan.ApplicationEffect {
		logInfo(rt.spec.Scope, "db %q: query hints stay advisory: %s", rt.spec.Name,
			hintPlan.Readiness())
	}
	tunerCfg := tunerConfig(cfg)
	result := tuner.New(rt.spec.Pool, tunerCfg, hintPlan, logStructuredWrapper)
	result.WithHostMemory(rt.hostMemory)
	timeouts := catalogread.FromSafety(cfg.Safety)
	result.WithPlanSource(tuning.PlanExplainer(rt.spec.Pool, rt.pgVersion(), timeouts), timeouts)
	// Every hint is planned plain and hinted before it is proposed (tuning-05).
	result.WithHintComparison(rt.pgVersion(), timeouts)
	if rt.collector != nil {
		result.WithQuerySource(rt.collector, rt.pgVersion())
	}
	rt.start(func() {
		result.StartRevalidationLoop(rt.ctx, tunerCfg.RevalidationIntervalHours)
	})
	rt.note("tuner")
	return result
}

func tunerConfig(c *config.Config) tuner.TunerConfig {
	return tuner.TunerConfig{
		Enabled:                   c.Tuner.Enabled,
		WorkMemMaxMB:              c.Tuner.WorkMemMaxMB,
		PlanTimeRatio:             c.Tuner.PlanTimeRatio,
		NestedLoopRowThreshold:    c.Tuner.NestedLoopRowThreshold,
		ParallelMinTableRows:      c.Tuner.ParallelMinTableRows,
		MinQueryCalls:             c.Tuner.MinQueryCalls,
		VerifyAfterApply:          c.Tuner.VerifyAfterApply,
		CascadeCooldownCycles:     c.Trust.CascadeCooldownCycles,
		PlanMaxAgeMinutes:         c.Tuner.PlanMaxAgeMinutes,
		ParamShapeMinVariants:     c.Tuner.ParamShapeMinVariants,
		HintCostTolerancePct:      c.Tuner.HintCostTolerancePct,
		HintRetirementDays:        c.Tuner.HintRetirementDays,
		RevalidationIntervalHours: c.Tuner.RevalidationIntervalHours,
		StaleStatsEstimateSkew:    c.Tuner.StaleStatsEstimateSkew,
		StaleStatsModRatio:        c.Tuner.StaleStatsModRatio,
		StaleStatsAgeMinutes:      c.Tuner.StaleStatsAgeMinutes,
		AnalyzeMaxTableMB:         c.Tuner.AnalyzeMaxTableMB,
		AnalyzeCooldownMinutes:    c.Tuner.AnalyzeCooldownMinutes,
		AnalyzeTimeoutMs:          c.Tuner.AnalyzeTimeoutMs,
		MaxConcurrentAnalyze:      c.Tuner.MaxConcurrentAnalyze,
	}
}
