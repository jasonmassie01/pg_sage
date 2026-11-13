package main

import (
	"context"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// market-08b wiring: a runtime with a log fanout feeds its budget monitor
// a cancel counter subscribed to its own database; without one the
// monitor has none (enforced timeouts then need approval).

// No concurrent access test: startBudgetCancels runs once per runtime,
// before the analyzer starts.

func budgetRuntime(t *testing.T, fanout *logwatch.LogFanout) (*databaseRuntime,
	*sync.WaitGroup) {
	t.Helper()
	saved := cfg
	cfg = config.DefaultConfig()
	t.Cleanup(func() { cfg = saved })
	ctx, cancel := context.WithCancel(context.Background())
	workers := &sync.WaitGroup{}
	t.Cleanup(func() { cancel(); workers.Wait() })
	rt := &databaseRuntime{ctx: ctx, workers: workers, logFanout: fanout,
		spec: databaseRuntimeSpec{Scope: "test", Name: "shop-fleet",
			Config: config.DatabaseConfig{Database: "shop"}}}
	rt.budgetMonitor = budget.NewMonitor(config.DefaultConfig(), nil, nil, nil,
		"shop-fleet", func(string, string, ...any) {})
	return rt, workers
}

func TestStartBudgetCancels_WiresTheCounterWithAFanout(t *testing.T) {
	fw := logwatch.NewFileWatcher(config.LogWatchConfig{LogDirectory: t.TempDir(),
		Format: "csvlog"}, func(string, string, ...any) {})
	rt, _ := budgetRuntime(t, logwatch.NewLogFanout(fw))
	rt.startBudgetCancels()
	found := false
	for _, f := range rt.features {
		found = found || f == "budget_cancels"
	}
	if !found {
		t.Fatalf("features = %v, want budget_cancels", rt.features)
	}
}

// Nil/empty: no fanout (no log access) and no monitor wire nothing.
func TestStartBudgetCancels_NoFanoutWiresNothing(t *testing.T) {
	rt, _ := budgetRuntime(t, nil)
	rt.startBudgetCancels()
	if len(rt.features) != 0 {
		t.Fatalf("features = %v, want none without a log fanout", rt.features)
	}
	rt.budgetMonitor = nil
	fw := logwatch.NewFileWatcher(config.LogWatchConfig{}, func(string, string, ...any) {})
	rt.logFanout = logwatch.NewLogFanout(fw)
	rt.startBudgetCancels()
	if len(rt.features) != 0 {
		t.Fatalf("features = %v, want none without a budget monitor", rt.features)
	}
}
