package main

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/startup"
)

func TestFleetLLMFeatureOwnersHonorIndependentEnableFlags(t *testing.T) {
	state, _, _ := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	cfg.LLM.Enabled = true
	cfg.LLM.APIKey = "fixture-key-never-used"
	cfg.LLM.Endpoint = "http://127.0.0.1:1/v1/chat/completions"
	cfg.LLM.FleetTokenBudgetDaily = 1000
	initializeFleetBudget([]string{"managed-a"})
	llmClient = llm.New(&cfg.LLM, nil)
	version := detectPGVersion(state.Pool)
	for _, enabled := range []bool{false, true} {
		cfg.Tuning.Enabled, cfg.Advisor.Enabled, cfg.Tuner.Enabled = enabled, enabled, enabled
		cfg.Tuner.LLMEnabled = enabled
		cfg.LLM.OptimizerLLM.Enabled = enabled
		cfg.LLM.OptimizerLLM.FallbackToGeneral = enabled
		rt := featureTestRuntime(t, state.Pool, version, true)
		opt, adv, tuner := rt.newTuningAgent(false, nil), rt.newAdvisor(), rt.newTuner()
		if (opt != nil) != enabled || (adv != nil) != enabled || (tuner != nil) != enabled {
			t.Fatalf("enabled=%v feature owners do not match flags", enabled)
		}
		if rt.generalLLM == nil || rt.llmManager == nil || rt.generalLLM == llmClient {
			t.Fatal("fleet reused global client or omitted per-database feature owners")
		}
	}
	// With LLM unavailable for the database, the LLM features are true nil
	// interfaces; the rule-based tuner still runs, as it always has in
	// standalone and YAML fleet (meta-db dropped it: G5-I07).
	rt := featureTestRuntime(t, state.Pool, version, false)
	opt, adv, tuner := rt.newTuningAgent(false, nil), rt.newAdvisor(), rt.newTuner()
	if opt != nil || adv != nil || rt.llmOn {
		t.Fatal("unavailable LLM left a live or typed-nil feature interface")
	}
	if tuner == nil {
		t.Fatal("the rule-based tuner was dropped with the LLM")
	}
}

// featureTestRuntime is a runtime shell for exercising feature builders;
// its workers stop when the test ends.
func featureTestRuntime(
	t *testing.T, pool *pgxpool.Pool, version int, llmAllowed bool,
) *databaseRuntime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rt := &databaseRuntime{
		spec: databaseRuntimeSpec{
			Scope: "fleet", Name: "managed-a", Pool: pool,
			Config: config.DatabaseConfig{LLMEnabled: &llmAllowed},
		},
		ctx: ctx, cancel: cancel, workers: &sync.WaitGroup{},
		checks: &startup.CheckResult{PGVersionNum: version},
	}
	rt.collector = collector.New(pool, cfg, version, nil)
	rt.resolveLLM()
	t.Cleanup(func() { cancel(); rt.workers.Wait() })
	return rt
}

func TestFleetBudgetIsolatedAndDisableClearsPriorOwner(t *testing.T) {
	prepareMetaGlobals(t)
	cfg.LLM.FleetTokenBudgetDaily = 100
	initializeFleetBudget([]string{"one", "two"})
	one, two := dbBudget{fleetLLMBudget, "one"}, dbBudget{fleetLLMBudget, "two"}
	if !one.CanSpend(40) || !two.CanSpend(40) {
		t.Fatal("fresh per-database allowance unavailable")
	}
	one.Spend(40)
	if one.CanSpend(20) || !two.CanSpend(40) {
		t.Fatal("one database spending leaked into another database allowance")
	}
	cfg.LLM.FleetTokenBudgetDaily = 0
	initializeFleetBudget(nil)
	if fleetLLMBudget != nil {
		t.Fatal("disabled budget retained previous owner")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resetFleetBudgetDaily(ctx, one.b)
}
