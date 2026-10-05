package main

import (
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/tuner"
	"github.com/pg-sage/sidecar/internal/tuning"
)

// newTuningAgent builds the database's tuning agent (roadmap 2.2) when
// tuning is enabled and its (possibly dedicated) LLM client is usable; the
// result is a true nil otherwise. hints is the query tuner the agent's
// hints go through (nil: no hints).
func (rt *databaseRuntime) newTuningAgent(autoExplain bool, hints *tuner.Tuner) *tuning.Agent {
	if !rt.tuningActive() {
		return nil
	}
	primary, fallback := tuningModels(rt.llmManager)
	timeouts := catalogread.FromSafety(cfg.Safety)
	options := []func(*optimizer.Optimizer){optimizer.WithCatalogReadTimeouts(timeouts)}
	if autoExplain {
		options = append(options, optimizer.WithAutoExplain())
	}
	deps := tuning.Deps{Model: primary,
		Indexes: optimizer.New(rt.spec.Pool, &cfg.LLM.Optimizer, rt.pgVersion(),
			logStructuredWrapper, options...),
		Store: tuning.NewPostgresStore(rt.spec.Pool, rt.pgVersion(), timeouts)}
	if fallback != nil {
		deps.Fallback = fallback
	}
	if rt.facts != nil {
		deps.Facts = rt.facts
	}
	if hints != nil {
		deps.Hints = hints
	}
	rt.note("tuning")
	settings := tuningSettings(cfg, rt.provider, rt.spec.Config.Database, 0)
	settings.HostMemory = rt.hostMemory
	return tuning.New(settings, deps, logStructuredWrapper)
}

// tuningActive reports whether this runtime builds the tuning agent:
// tuning.enabled and a usable model for it.
func (rt *databaseRuntime) tuningActive() bool {
	if !cfg.Tuning.Enabled || !rt.llmOn {
		return false
	}
	primary, _ := tuningModels(rt.llmManager)
	return primary != nil && primary.IsEnabled()
}

// tuningModels returns the agent's model (the optimizer-purpose client)
// and its fallback. The fallback is nil when it would be the primary
// itself: retrying a failed call on the same client doubles latency and
// circuit-breaker failures.
func tuningModels(manager *llm.Manager) (primary, fallback *llm.Client) {
	if manager == nil {
		return nil, nil
	}
	primary = manager.ForPurpose("index_optimization")
	if cfg.LLM.OptimizerLLM.FallbackToGeneral && manager.General != nil &&
		manager.General != primary {
		fallback = manager.General
	}
	return primary, fallback
}

// tuningSettings are the agent's settings: its own budgets (tuning.*) and
// the existing switches, which decide the proposal types it may make.
// Extended statistics are always allowed (they only add planner
// statistics, verified by the statistics verifier).
func tuningSettings(c *config.Config, cloudEnv, dbName string, hostMem int64) tuning.Settings {
	index := c.LLM.Optimizer.Enabled
	return tuning.Settings{
		Tuning:              c.Tuning,
		ConfidenceThreshold: c.LLM.Optimizer.ConfidenceThreshold,
		Memory:              c.LLM.Optimizer.RejectionMemory,
		Allowed: map[tuning.ProposalType]bool{
			tuning.ProposeIndexCreate: index,
			tuning.ProposeIndexDrop:   index,
			tuning.ProposeGUC:         c.Advisor.Enabled && c.Advisor.MemoryEnabled,
			tuning.ProposeReloption:   c.Advisor.Enabled && c.Advisor.VacuumEnabled,
			tuning.ProposeStatistics:  true,
			tuning.ProposeQueryHint:   c.Tuner.Enabled && c.Tuner.LLMEnabled,
		},
		CloudEnv:        cloudEnv,
		DatabaseName:    dbName,
		HostMemoryBytes: hostMem,
		MaxOutputTokens: c.LLM.OptimizerLLM.MaxOutputTokens,
		Thresholds:      tuning.DefaultThresholds(),
		MaxNewPerTable:  c.LLM.Optimizer.MaxNewPerTable,
		DailyTokenLimit: tuningDailyTokens(c),
	}
}

// tuningDailyTokens is the agent's durable daily token cap: the daily
// budget of the client it uses (the optimizer LLM's when that is enabled
// and has its own, else the general one), charged per database and UTC
// day in sage.tuning_budget_day so a restart does not reset it.
func tuningDailyTokens(c *config.Config) int64 {
	if c.LLM.OptimizerLLM.Enabled && c.LLM.OptimizerLLM.TokenBudgetDaily > 0 {
		return int64(c.LLM.OptimizerLLM.TokenBudgetDaily)
	}
	return int64(c.LLM.TokenBudgetDaily)
}
