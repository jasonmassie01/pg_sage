package changeplan

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// plan-02b: the caps a plan is checked against when it is proposed and
// approved (CheckPlanCaps) and before each step (the engine, caps_db_test).
// No concurrent access tests here: these are pure functions; the
// per-database concurrency case is in caps_db_test.go.

func TestCheckPlanCaps_TwoIndexesOnATableAndASettingPass(t *testing.T) {
	if err := CheckPlanCaps(threeStepPlan(), policy.PlanCaps{}, testSettings); err != nil {
		t.Fatalf("two indexes and a reloadable setting refused: %v", err)
	}
}

// Boundary: exactly 2 new indexes on one table pass, 3 are refused with
// the cap, the table and the numbers; indexes spread over tables pass.
func TestCheckPlanCaps_IndexesPerTableBoundary(t *testing.T) {
	if err := CheckPlanCaps(chain(2, 101), policy.PlanCaps{}, testSettings); err != nil {
		t.Fatalf("2 indexes on one table refused: %v", err)
	}
	err := CheckPlanCaps(chain(3, 101), policy.PlanCaps{}, testSettings)
	var capErr *CapError
	if !errors.As(err, &capErr) || !errors.Is(err, ErrOverCap) ||
		capErr.Cap != CapIndexesPerTable || capErr.Limit != 2 || capErr.Got != 3 ||
		capErr.Object != "public.orders" {
		t.Fatalf("3 indexes on one table: %v (%+v), want the per-table cap 3 > 2", err, capErr)
	}
	for _, want := range []string{"max_new_indexes_per_table", "public.orders", "3", "2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	spread := chain(3, 101)
	spread.Steps[2].ProposedSQL = "CREATE INDEX CONCURRENTLY idx_c ON public.lines (c3)"
	if err := CheckPlanCaps(spread, policy.PlanCaps{}, testSettings); err != nil {
		t.Fatalf("2 + 1 indexes on two tables refused: %v", err)
	}
	if err := CheckPlanCaps(chain(3, 101), policy.PlanCaps{MaxNewIndexesPerTable: 3},
		testSettings); err != nil {
		t.Fatalf("3 indexes under a cap of 3 refused: %v", err)
	}
}

// More than one restart-bound step is refused; one is allowed by the cap
// (and then needs its own approval, classify.go).
func TestCheckPlanCaps_RestartBoundSteps(t *testing.T) {
	p := chain(1, 101)
	restart := func(ordinal int, name string) Step {
		s := settingStep(ordinal)
		s.Setting = name
		s.ProposedSQL = "ALTER SYSTEM SET " + name + " = 300"
		s.RollbackSQL = "ALTER SYSTEM RESET " + name
		return s
	}
	p.Steps = append(p.Steps, restart(2, "max_connections"))
	if err := CheckPlanCaps(p, policy.PlanCaps{}, testSettings); err != nil {
		t.Fatalf("one restart-bound step refused: %v", err)
	}
	p.Steps = append(p.Steps, restart(3, "shared_buffers"))
	err := CheckPlanCaps(p, policy.PlanCaps{}, testSettings)
	var capErr *CapError
	if !errors.As(err, &capErr) || capErr.Cap != CapRestartBoundSteps || capErr.Got != 2 ||
		capErr.Limit != 1 {
		t.Fatalf("two restart-bound steps: %v, want the restart cap 2 > 1", err)
	}
}

// Nil/empty: a plan with no steps, or only check steps, passes; a setting
// of unknown context counts as restart-bound (fail closed).
func TestCheckPlanCaps_EmptyPlansAndUnknownSettings(t *testing.T) {
	if err := CheckPlanCaps(Plan{}, policy.PlanCaps{}, nil); err != nil {
		t.Fatalf("empty plan refused: %v", err)
	}
	p := chain(1, 101)
	p.Steps = []Step{checkStep(1, ""), checkStep(2, "")}
	if err := CheckPlanCaps(p, policy.PlanCaps{}, nil); err != nil {
		t.Fatalf("check-only plan refused: %v", err)
	}
	q := chain(0, 101)
	q.Steps = []Step{settingStep(1), settingStep(2)}
	if err := CheckPlanCaps(q, policy.PlanCaps{}, nil); !errors.Is(err, ErrOverCap) {
		t.Fatalf("two settings of unknown context: %v, want the restart cap", err)
	}
}

func TestNewIndexOf_ParsesTheIndexAndItsTable(t *testing.T) {
	cases := []struct {
		sql, index, table string
		ok                bool
	}{
		{"CREATE INDEX CONCURRENTLY idx_a ON public.orders (a)", "public.idx_a",
			"public.orders", true},
		{"create unique index concurrently if not exists IDX_B on Orders using btree (b)",
			"public.idx_b", "public.orders", true},
		{"CREATE INDEX CONCURRENTLY app.idx_c ON ONLY app.lines (c)", "app.idx_c",
			"app.lines", true},
		{`CREATE INDEX CONCURRENTLY "Idx D" ON sales."Lines" (d)`, `sales."Idx D"`,
			`sales."Lines"`, true},
		{"CREATE INDEX CONCURRENTLY ON public.orders (e)", "", "public.orders", true},
		{"DROP INDEX CONCURRENTLY public.idx_a", "", "", false},
		{"ALTER SYSTEM SET random_page_cost = 1.1", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		index, table, ok := newIndexOf(Step{Kind: KindAction, ProposedSQL: c.sql})
		if index != c.index || table != c.table || ok != c.ok {
			t.Errorf("%q: (%q, %q, %v), want (%q, %q, %v)", c.sql, index, table, ok,
				c.index, c.table, c.ok)
		}
	}
	if _, _, ok := newIndexOf(Step{Kind: KindCheck,
		ProposedSQL: "CREATE INDEX CONCURRENTLY i ON t (a)"}); ok {
		t.Fatal("a check step builds no index")
	}
}

// Boundary: the per-database count before a step counts the other plans'
// started steps on the same table.
func TestIndexCountWait_CountsEveryPlansStepsOnTheTable(t *testing.T) {
	s := indexStep(1, "idx_new", "x", 11, 101)
	other := func(sql string) capStep { return capStep{planID: 9, sql: sql} }
	one := []capStep{other("CREATE INDEX CONCURRENTLY idx_o1 ON public.orders (a)"),
		other("CREATE INDEX CONCURRENTLY idx_l1 ON public.lines (a)")}
	if why := indexCountWait(s, one, policy.PlanCaps{}); why != "" {
		t.Fatalf("1 + 1 on public.orders waits: %s", why)
	}
	two := append(one, other("CREATE INDEX CONCURRENTLY idx_o2 ON public.orders (b)"))
	why := indexCountWait(s, two, policy.PlanCaps{})
	if !strings.HasPrefix(why, CapIndexesPerTable+":") || !strings.Contains(why, "3") ||
		!strings.Contains(why, "public.orders") {
		t.Fatalf("2 + 1 on public.orders: %q, want the per-table cap", why)
	}
	if why := indexCountWait(settingStep(1), two, policy.PlanCaps{}); why != "" {
		t.Fatalf("a setting step waits on the index cap: %s", why)
	}
}

// Boundary: built bytes at exactly the cap pass, one byte over waits.
func TestIndexBytesWait_Boundary(t *testing.T) {
	if why := indexBytesWait(1000, 10000, policy.PlanCaps{}); why != "" {
		t.Fatalf("exactly 10%%: %s", why)
	}
	why := indexBytesWait(1001, 10000, policy.PlanCaps{})
	if !strings.HasPrefix(why, CapIndexBytesPct+":") || !strings.Contains(why, "1001") ||
		!strings.Contains(why, "1000") {
		t.Fatalf("one byte over: %q, want the byte cap with the numbers", why)
	}
	if why := indexBytesWait(5000, 10000, policy.PlanCaps{MaxNewIndexBytesPct: 50}); why != "" {
		t.Fatalf("50%% under a cap of 50%%: %s", why)
	}
}
