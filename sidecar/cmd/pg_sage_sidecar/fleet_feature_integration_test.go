package main

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/llm"
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
	coll := collector.New(state.Pool, cfg, version, nil)
	for _, enabled := range []bool{false, true} {
		cfg.LLM.Optimizer.Enabled, cfg.Advisor.Enabled, cfg.Tuner.Enabled = enabled, enabled, enabled
		cfg.Tuner.LLMEnabled = enabled
		cfg.LLM.OptimizerLLM.Enabled = enabled
		cfg.LLM.OptimizerLLM.FallbackToGeneral = enabled
		opt, adv, tuner, brief, client, manager :=
			buildFleetLLMFeatures(state.Pool, version, coll, "managed-a")
		if (opt != nil) != enabled || (adv != nil) != enabled || (tuner != nil) != enabled {
			t.Fatalf("enabled=%v feature owners do not match flags", enabled)
		}
		if brief == nil || client == nil || manager == nil || client == llmClient {
			t.Fatal("fleet reused global client or omitted per-database feature owners")
		}
	}
	llmClient = nil
	opt, adv, tuner, brief, client, manager :=
		buildFleetLLMFeatures(state.Pool, version, coll, "managed-a")
	if opt != nil || adv != nil || tuner != nil || brief != nil || client != nil || manager != nil {
		t.Fatal("unavailable global LLM left a live or typed-nil feature interface")
	}
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
