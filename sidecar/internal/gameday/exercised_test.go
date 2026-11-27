package gameday

import (
	"strings"
	"testing"
)

// benches-12: a game day records what it exercised: the provider,
// whether the clone holds customer objects, each scenario of the report
// and each customer program's binding or skip reason.
//
// No concurrent access tests: exercisedOf and its readers are pure
// functions over their arguments.

const customerReport = `{"runs": [
	{"scenario": "lock-hot-row", "arm": "causal-graph"},
	{"scenario": "lock-hot-row", "arm": "causal-graph"},
	{"scenario": "lock-prepared-holder", "arm": "causal-graph",
	 "skipped": "max_prepared_transactions is 0"},
	{"scenario": "lock_customer_hot_table", "arm": "causal-graph"}]}`

func boundOrders() Binding {
	return Binding{Program: ProgramCustomerHotTable, Schema: "public", Table: "orders"}
}

func TestExercisedRecordsScenariosAndTheBinding(t *testing.T) {
	x := exercisedOf("dle", []Binding{boundOrders()}, []byte(customerReport))
	if x.Provider != "dle" || !x.CustomerObjects || len(x.Scenarios) != 3 {
		t.Fatalf("exercised = %+v", x)
	}
	want := []ExercisedScenario{
		{ID: "lock-hot-row", Runs: 2, Scored: 2},
		{ID: "lock-prepared-holder", Runs: 1, Skip: "max_prepared_transactions is 0"},
		{ID: ProgramCustomerHotTable, Runs: 1, Scored: 1, Binding: `"public"."orders"`,
			BindingHash: boundOrders().Hash()},
	}
	for i, w := range want {
		if x.Scenarios[i] != w {
			t.Errorf("scenario %d = %+v, want %+v", i, x.Scenarios[i], w)
		}
	}
	if _, unscored := customerUnscored(x); unscored {
		t.Fatal("a scored customer program reads as unscored")
	}
	if err := customerFailure(x); err != nil {
		t.Fatalf("customerFailure = %v, want none", err)
	}
}

// The local provider: no customer objects, and the customer program's
// skip reason (from the binding) is what the record shows.
func TestExercisedOnLocalShowsTheSkip(t *testing.T) {
	skip := Binding{Program: ProgramCustomerHotTable, Skip: SkipLocalProvider}
	raw := `{"runs": [{"scenario": "lock-hot-row", "arm": "causal-graph"},
		{"scenario": "lock_customer_hot_table", "arm": "causal-graph",
		 "skipped": "the local provider's target holds no customer objects"}]}`
	x := exercisedOf("local", []Binding{skip}, []byte(raw))
	if x.CustomerObjects {
		t.Fatal("a local target reads as holding customer objects")
	}
	sc, unscored := customerUnscored(x)
	if !unscored || sc.ID != ProgramCustomerHotTable || sc.Skip != SkipLocalProvider ||
		sc.Binding != "" || sc.BindingHash != "" {
		t.Fatalf("customerUnscored = %+v, %v; want the local skip", sc, unscored)
	}
}

// Nil/empty: a run without a report still records the bindings; an
// unreadable report records no scenario from it.
func TestExercisedWithoutAReportKeepsTheBindings(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("not json"), []byte(`{"runs": []}`)} {
		x := exercisedOf("dle", []Binding{boundOrders()}, raw)
		if len(x.Scenarios) != 1 || x.Scenarios[0].ID != ProgramCustomerHotTable ||
			x.Scenarios[0].Runs != 0 || x.Scenarios[0].BindingHash == "" {
			t.Fatalf("raw %q: exercised = %+v", raw, x)
		}
		// Not in the report (its family was not selected): neither scored nor
		// failed, so the fixtures decide the status.
		if _, unscored := customerUnscored(x); unscored {
			t.Fatalf("raw %q: an absent customer program reads as unscored", raw)
		}
	}
	x := exercisedOf("dle", nil, nil)
	if x.Scenarios == nil || len(x.Scenarios) != 0 {
		t.Fatalf("no bindings, no report: scenarios = %#v, want empty", x.Scenarios)
	}
}

// Error propagation: an errored customer program fails the game day with
// its error (the SQLSTATE), even when the fixtures scored.
func TestCustomerFailureCarriesTheError(t *testing.T) {
	raw := `{"runs": [{"scenario": "lock-hot-row", "arm": "causal-graph"},
		{"scenario": "lock_customer_hot_table", "arm": "causal-graph",
		 "error": "inject: holder lock on the bound table: SQLSTATE 42501"}]}`
	x := exercisedOf("dle", []Binding{boundOrders()}, []byte(raw))
	err := customerFailure(x)
	if err == nil || !strings.Contains(err.Error(), "SQLSTATE 42501") ||
		!strings.Contains(err.Error(), ProgramCustomerHotTable) {
		t.Fatalf("customerFailure = %v, want the program and its SQLSTATE", err)
	}
	if x.Scenarios[1].Error == "" || x.Scenarios[1].Scored != 0 {
		t.Fatalf("customer scenario = %+v, want the error recorded", x.Scenarios[1])
	}
}

// A fixture's error or skip never fails or unscores the game day here:
// that stays benches-08's rule over the whole report.
func TestFixtureProblemsAreNotCustomerProblems(t *testing.T) {
	raw := `{"runs": [{"scenario": "lock-hot-row", "arm": "causal-graph", "error": "boom"},
		{"scenario": "lock_customer_hot_table", "arm": "causal-graph"}]}`
	x := exercisedOf("dle", []Binding{boundOrders()}, []byte(raw))
	if err := customerFailure(x); err != nil {
		t.Fatalf("a fixture error failed the customer program: %v", err)
	}
	if _, unscored := customerUnscored(x); unscored {
		t.Fatal("a fixture error unscored the customer program")
	}
}
