package main

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// fleet-06: the fleet budget initializeFleetBudget builds is the one the
// fleet status (GET /api/v1/databases) reports.

func budgetStatusGlobals(t *testing.T) {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	oldCtx := shutdownCtx
	ctx, cancel := context.WithCancel(context.Background())
	shutdownCtx = ctx
	cfg = config.DefaultConfig()
	cfg.Mode = "fleet"
	fleetMgr = fleet.NewManager(cfg)
	t.Cleanup(func() { cancel(); shutdownCtx = oldCtx })
}

func budgetRow(t *testing.T, name string) *fleet.BudgetUsage {
	t.Helper()
	row, ok := fleetMgr.InstanceStatus(name)
	if !ok {
		t.Fatalf("database %q not registered", name)
	}
	return row.LLMBudget
}

func TestInitializeFleetBudgetFeedsFleetStatus(t *testing.T) {
	budgetStatusGlobals(t)
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "one",
		Status: &fleet.InstanceStatus{}})
	cfg.LLM.FleetTokenBudgetDaily = 900
	initializeFleetBudget([]string{"one", "two", "three"})
	fleetLLMBudget.Spend("one", 42)
	got := budgetRow(t, "one")
	if got == nil || got.Allocation != 300 || got.Used != 42 {
		t.Fatalf("status budget = %+v, want allocation 300 used 42", got)
	}
}

func TestInitializeFleetBudgetDisabledClearsStatus(t *testing.T) {
	budgetStatusGlobals(t)
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{Name: "one",
		Status: &fleet.InstanceStatus{}})
	cfg.LLM.FleetTokenBudgetDaily = 100
	initializeFleetBudget([]string{"one"})
	if budgetRow(t, "one") == nil {
		t.Fatal("status lacks the enabled budget")
	}
	cfg.LLM.FleetTokenBudgetDaily = 0
	initializeFleetBudget(nil)
	if got := budgetRow(t, "one"); got != nil {
		t.Errorf("status budget = %+v after the budget was disabled, want nil", *got)
	}
}

func TestInitializeFleetBudgetWithoutManager(t *testing.T) {
	// Nil: a budget built before any fleet manager exists must not panic.
	budgetStatusGlobals(t)
	fleetMgr = nil
	cfg.LLM.FleetTokenBudgetDaily = 100
	initializeFleetBudget([]string{"one"})
	if fleetLLMBudget == nil || fleetLLMBudget.Allocation("one") != 100 {
		t.Fatalf("budget not built without a manager: %+v", fleetLLMBudget)
	}
}
