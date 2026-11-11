package fleet

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// fleet-06: GET /api/v1/databases carries each database's LLM budget
// (allocation and use) when a fleet budget is configured.

func budgetManager(t *testing.T, names ...string) *DatabaseManager {
	t.Helper()
	mgr := NewManager(config.DefaultConfig())
	for _, name := range names {
		mgr.RegisterInstance(&DatabaseInstance{Name: name,
			Status: &InstanceStatus{Connected: true}})
	}
	return mgr
}

func statusByName(t *testing.T, overview FleetOverview) map[string]DatabaseStatus {
	t.Helper()
	out := make(map[string]DatabaseStatus, len(overview.Databases))
	for _, db := range overview.Databases {
		out[db.Name] = db
	}
	return out
}

func TestFleetStatusBudget_ReportsEachAllocation(t *testing.T) {
	mgr := budgetManager(t, "a", "b", "c")
	budget := NewBudget(3000, []string{"a", "b", "c"})
	budget.Spend("b", 250)
	mgr.SetBudgetSource(budget)

	rows := statusByName(t, mgr.FleetStatus())
	if len(rows) != 3 {
		t.Fatalf("FleetStatus listed %d databases, want 3", len(rows))
	}
	want := map[string]int{"a": 0, "b": 250, "c": 0}
	for name, used := range want {
		got := rows[name].LLMBudget
		if got == nil {
			t.Fatalf("database %q has no llm_budget", name)
		}
		if got.Allocation != 1000 || got.Used != used || got.Weight != 1 {
			t.Errorf("database %q budget = %+v, want allocation 1000 used %d weight 1",
				name, *got, used)
		}
	}
	raw, err := json.Marshal(rows["b"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"llm_budget":{"allocation":1000,"used":250`) {
		t.Errorf("row JSON lacks the budget: %s", raw)
	}
}

func TestFleetStatusBudget_NoSourceOmitsField(t *testing.T) {
	mgr := budgetManager(t, "a")
	rows := statusByName(t, mgr.FleetStatus())
	if rows["a"].LLMBudget != nil {
		t.Fatalf("llm_budget = %+v without a fleet budget, want nil", rows["a"].LLMBudget)
	}
	raw, err := json.Marshal(mgr.FleetStatus())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "llm_budget") {
		t.Errorf("overview JSON names llm_budget without a fleet budget: %s", raw)
	}
}

func TestFleetStatusBudget_ClearedSourceOmitsField(t *testing.T) {
	mgr := budgetManager(t, "a")
	mgr.SetBudgetSource(NewBudget(100, []string{"a"}))
	if statusByName(t, mgr.FleetStatus())["a"].LLMBudget == nil {
		t.Fatal("llm_budget missing while the budget is set")
	}
	mgr.SetBudgetSource(nil)
	if got := statusByName(t, mgr.FleetStatus())["a"].LLMBudget; got != nil {
		t.Errorf("llm_budget = %+v after the budget was cleared, want nil", *got)
	}
}

func TestFleetStatusBudget_NilFleetBudgetIsNoBudget(t *testing.T) {
	// A typed-nil *FleetBudget (budget disabled) must not panic the
	// status read and must read as "no budget".
	mgr := budgetManager(t, "a")
	var disabled *FleetBudget
	mgr.SetBudgetSource(disabled)
	if got := statusByName(t, mgr.FleetStatus())["a"].LLMBudget; got != nil {
		t.Errorf("llm_budget = %+v from a nil budget, want nil", *got)
	}
	if snap := disabled.Snapshot(); snap != nil {
		t.Errorf("nil budget Snapshot = %v, want nil", snap)
	}
}

func TestFleetStatusBudget_DatabaseOutsideBudgetHasNone(t *testing.T) {
	mgr := budgetManager(t, "a", "late")
	mgr.SetBudgetSource(NewBudget(100, []string{"a"}))
	rows := statusByName(t, mgr.FleetStatus())
	if rows["a"].LLMBudget == nil || rows["a"].LLMBudget.Allocation != 100 {
		t.Fatalf("a budget = %+v, want allocation 100", rows["a"].LLMBudget)
	}
	if rows["late"].LLMBudget != nil {
		t.Errorf("unregistered database budget = %+v, want nil", *rows["late"].LLMBudget)
	}
}

func TestFleetStatusBudget_ZeroAllocationIsReported(t *testing.T) {
	// Boundary: a cap smaller than the database count leaves each share at
	// 0; the row must say 0, not omit the budget.
	mgr := budgetManager(t, "a", "b", "c")
	mgr.SetBudgetSource(NewBudget(2, []string{"a", "b", "c"}))
	for name, row := range statusByName(t, mgr.FleetStatus()) {
		if row.LLMBudget == nil || row.LLMBudget.Allocation != 0 {
			t.Errorf("database %q budget = %+v, want allocation 0", name, row.LLMBudget)
		}
	}
}

func TestInstanceStatusCarriesBudget(t *testing.T) {
	mgr := budgetManager(t, "a", "b")
	budget := NewBudget(500, []string{"a", "b"})
	budget.Spend("a", 7)
	mgr.SetBudgetSource(budget)
	row, ok := mgr.InstanceStatus("a")
	if !ok {
		t.Fatal("InstanceStatus did not find a")
	}
	if row.LLMBudget == nil || row.LLMBudget.Allocation != 250 || row.LLMBudget.Used != 7 {
		t.Errorf("InstanceStatus budget = %+v, want allocation 250 used 7", row.LLMBudget)
	}
}

func TestFleetStatusBudget_RebalanceIsVisible(t *testing.T) {
	// State transition: registering a database rebalances every share and
	// the next status read shows the new split.
	mgr := budgetManager(t, "a", "b")
	budget := NewBudget(1000, []string{"a"})
	mgr.SetBudgetSource(budget)
	if got := statusByName(t, mgr.FleetStatus())["a"].LLMBudget; got.Allocation != 1000 {
		t.Fatalf("before register a allocation = %d, want 1000", got.Allocation)
	}
	budget.Register("b")
	rows := statusByName(t, mgr.FleetStatus())
	if rows["a"].LLMBudget.Allocation != 500 || rows["b"].LLMBudget == nil ||
		rows["b"].LLMBudget.Allocation != 500 {
		t.Errorf("after register a=%+v b=%+v, want 500 each", rows["a"].LLMBudget,
			rows["b"].LLMBudget)
	}
}

func TestFleetStatusBudget_ConcurrentRebalance(t *testing.T) {
	// Run with -race where cgo is available: FleetStatus reads the budget
	// while Register/Unregister rebalance it.
	names := []string{"a", "b", "c"}
	mgr := budgetManager(t, names...)
	budget := NewBudget(3000, names)
	mgr.SetBudgetSource(budget)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				extra := fmt.Sprintf("extra-%d-%d", w, i%5)
				budget.Register(extra)
				budget.Unregister(extra)
			}
		}(w)
	}
	errs := make(chan string, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			for _, row := range mgr.FleetStatus().Databases {
				if row.LLMBudget == nil || row.LLMBudget.Allocation <= 0 ||
					row.LLMBudget.Allocation > 3000 {
					select {
					case errs <- fmt.Sprintf("%s budget %+v", row.Name, row.LLMBudget):
					default:
					}
				}
			}
		}
	}()
	wg.Wait()
	close(errs)
	if msg, ok := <-errs; ok {
		t.Errorf("inconsistent budget during rebalance: %s", msg)
	}
	for name, row := range statusByName(t, mgr.FleetStatus()) {
		if row.LLMBudget == nil || row.LLMBudget.Allocation != 1000 {
			t.Errorf("after rebalancing %q = %+v, want allocation 1000", name, row.LLMBudget)
		}
	}
}
