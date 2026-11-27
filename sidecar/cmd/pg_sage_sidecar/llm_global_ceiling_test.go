package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-03 part 2: the registry derives one process-wide ceiling from its
// clients and enforces it across all of them.

func TestGlobalCeiling_DefaultFleetDerivesEveryClient(t *testing.T) {
	llmTestGlobals(t)
	out := captureStderr(t, func() {
		newFleetDBLLMClients("orders", true)
		newFleetDBLLMClients("billing", true)
	})
	// 2 databases x (general + optimizer) + the shared client.
	want := int64(5 * config.DefaultLLMTokenBudget)
	status := llmClients.GlobalBudget()
	if status.Ceiling != want || status.Source != globalSourceDerived {
		t.Fatalf("global = %+v, want ceiling %d from %q", status, want,
			globalSourceDerived)
	}
	if !strings.Contains(out, "2500000 tokens/day") ||
		!strings.Contains(out, "5 clients") {
		t.Fatalf("startup log does not name the derived ceiling:\n%s", out)
	}
	var reporter api.LLMGlobalBudgetReporter = llmClients
	raw, err := json.Marshal(map[string]any{"global": reporter.GlobalBudget()})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"ceiling":2500000`, `"used":0`, `"source":"derived"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("status JSON %s lacks %s", raw, key)
		}
	}
}

func TestGlobalCeiling_UnlimitedClientMakesItUnlimited(t *testing.T) {
	llmTestGlobals(t)
	newFleetDBLLMClients("orders", true)
	// token_budget_daily is restart-bound; drive the owner's commit path.
	reconfigureRegistry(func(l *config.LLMConfig) {
		l.TokenBudgetDaily, l.OptimizerLLM.TokenBudgetDaily = 0, 0
	})
	if s := llmClients.GlobalBudget(); s.Ceiling != 0 || s.Source != globalSourceDerived {
		t.Fatalf("global = %+v, want unlimited (0) derived", s)
	}
}

// fleet_token_budget_daily is inert without a running fleet budget (a
// standalone sidecar), so it must not lower the derived ceiling there.
func TestGlobalCeiling_InertFleetKeyChangesNothing(t *testing.T) {
	llmTestGlobals(t)
	newFleetDBLLMClients("orders", true)
	reconfigureRegistry(func(l *config.LLMConfig) { l.FleetTokenBudgetDaily = 300000 })
	if s := llmClients.GlobalBudget(); s.Source != globalSourceDerived ||
		s.Ceiling != int64(3*config.DefaultLLMTokenBudget) {
		t.Fatalf("global = %+v, want derived 3 x default with no fleet budget", s)
	}
}

// An active fleet budget bounds the per-database clients by its total;
// the shared client keeps its own budget on top, so the derived ceiling
// is still today's effective total. The explicit key wins over both.
func TestGlobalCeiling_FleetBudgetAndExplicitKeyWin(t *testing.T) {
	llmTestGlobals(t)
	cfg.LLM.FleetTokenBudgetDaily = 300000
	fleetLLMBudget = fleet.NewBudget(300000, []string{"orders"})
	reconfigureRegistry(func(l *config.LLMConfig) { l.FleetTokenBudgetDaily = 300000 })
	newFleetDBLLMClients("orders", true)
	newFleetDBLLMClients("billing", true)
	want := int64(300000 + config.DefaultLLMTokenBudget)
	if s := llmClients.GlobalBudget(); s.Ceiling != want || s.Source != globalSourceFleet {
		t.Fatalf("global = %+v, want fleet 300000 + shared %d", s,
			config.DefaultLLMTokenBudget)
	}
	// An unlimited shared client still makes the derived ceiling unlimited.
	reconfigureRegistry(func(l *config.LLMConfig) {
		l.FleetTokenBudgetDaily, l.TokenBudgetDaily = 300000, 0
	})
	if s := llmClients.GlobalBudget(); s.Ceiling != 0 {
		t.Fatalf("global = %+v, want unlimited with an unlimited shared client", s)
	}
	// llm.global_token_budget_daily is hot-reloadable through the controller.
	applyLLMChange(t, func(l *config.LLMConfig) {
		l.GlobalTokenBudgetDaily, l.FleetTokenBudgetDaily = 1234, 300000
	})
	if s := llmClients.GlobalBudget(); s.Ceiling != 1234 || s.Source != globalSourceConfigured {
		t.Fatalf("global = %+v, want the configured 1234", s)
	}
	fleetLLMBudget = nil
	reconfigureRegistry(func(l *config.LLMConfig) {
		l.GlobalTokenBudgetDaily, l.FleetTokenBudgetDaily = 0, 0
	})
	if s := llmClients.GlobalBudget(); s.Source != globalSourceDerived ||
		s.Ceiling != int64(5*config.DefaultLLMTokenBudget) {
		t.Fatalf("global = %+v, want derived 5 x default after clearing", s)
	}
}

// reconfigureRegistry commits an llm.* change through the registry, the
// path the config controller takes for the "llm" owner.
func reconfigureRegistry(mutate func(*config.LLMConfig)) {
	parent := configController.Desired().Config.LLM
	mutate(&parent)
	llmClients.reconfigure(parent)
}

// The ceiling follows the registry: a removed runtime's budget leaves it.
func TestGlobalCeiling_RecomputedOnRemove(t *testing.T) {
	llmTestGlobals(t)
	general, manager := newFleetDBLLMClients("orders", true)
	llmClients.remove(general, manager.Optimizer)
	if s := llmClients.GlobalBudget(); s.Ceiling != int64(config.DefaultLLMTokenBudget) {
		t.Fatalf("global = %+v, want only the shared client's budget", s)
	}
}

// Every registry client draws on one budget, and ResetBudgets zeroes it.
func TestGlobalCeiling_SharedAcrossClientsAndReset(t *testing.T) {
	llmTestGlobals(t)
	pointLLMAt(t, fakeLLMProvider(t, 40).URL)
	applyLLMChange(t, func(l *config.LLMConfig) { l.GlobalTokenBudgetDaily = 100 })
	dbGeneral, _ := newFleetDBLLMClients("orders", true)
	ctx := context.Background()
	if _, _, err := dbGeneral.Chat(ctx, "sys", "per-database", 40); err != nil {
		t.Fatalf("per-database chat: %v", err)
	}
	if _, _, err := llmClient.Chat(ctx, "sys", "shared", 40); err != nil {
		t.Fatalf("shared chat: %v", err)
	}
	if got := llmClients.GlobalBudget().Used; got != 80 {
		t.Fatalf("global used = %d, want 80 across both clients", got)
	}
	_, _, err := dbGeneral.Chat(ctx, "sys", "over the ceiling", 40)
	if !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("80 + 40 under a 100 ceiling = %v, want ErrBudgetExhausted", err)
	}
	llmClients.ResetBudgets()
	if got := llmClients.GlobalBudget().Used; got != 0 {
		t.Fatalf("global used after reset = %d, want 0", got)
	}
}
