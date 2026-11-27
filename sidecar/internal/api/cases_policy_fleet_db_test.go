package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// CHECK-01: one /api/v1/cases request explains policy once per selected
// database, however many cases each database has.
func TestCasesPolicyExplainsOncePerSelectedDatabase(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	seedCountFindings(t, ctx, 40)
	cfg := caseAutonomousConfig()
	mgr := fleet.NewManager(cfg)
	gates := map[string]*countingBatchGate{}
	for _, name := range []string{"alpha", "beta"} {
		inst, gate := countingCaseInstance(cfg, nil)
		inst.Name, inst.Config = name, config.DatabaseConfig{Name: name, ExecutionMode: "auto"}
		inst.Pool = pool
		mgr.RegisterInstance(inst)
		gates[name] = gate
	}
	wantTypes := []string{"vacuum_table", "create_index_concurrently", "analyze_table"}

	all, err := queryProjectedCases(ctx, mgr, "all")
	if err != nil {
		t.Fatalf("queryProjectedCases(all): %v", err)
	}
	if len(all) != 2*(40+2) {
		t.Fatalf("fleet cases = %d, want %d (40 findings + 2 incidents per database)",
			len(all), 2*(40+2))
	}
	for name, gate := range gates {
		calls := gate.calls()
		if len(calls) != 1 {
			t.Fatalf("%s explain calls = %d, want 1 for the fleet request", name, len(calls))
		}
		// cases-04: the one call carries each finding's statement (a type
		// repeats per table), no longer each type once.
		if !containsAll(calls[0], wantTypes) {
			t.Fatalf("%s explained %v, want all of %v", name, calls[0], wantTypes)
		}
	}

	if _, err := queryProjectedCases(ctx, mgr, "beta"); err != nil {
		t.Fatalf("queryProjectedCases(beta): %v", err)
	}
	if a, b := len(gates["alpha"].calls()), len(gates["beta"].calls()); a != 1 || b != 2 {
		t.Fatalf("after a beta-only request calls = alpha %d / beta %d, want 1 / 2", a, b)
	}
}

func containsAll(got, want []string) bool {
	seen := map[string]bool{}
	for _, v := range got {
		seen[v] = true
	}
	for _, v := range want {
		if !seen[v] {
			return false
		}
	}
	return true
}
