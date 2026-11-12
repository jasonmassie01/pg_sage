package config

import (
	"strings"
	"testing"
)

// market-08: the budgets list and advisor.budget_proposals. No concurrent
// access tests: validation is a pure function of the configuration.

// Defaults: no budgets, proposals on, with no configuration file at all.
func TestBudgetDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if len(cfg.Budgets) != 0 {
		t.Fatalf("default budgets = %+v, want none", cfg.Budgets)
	}
	if !cfg.Advisor.BudgetProposals {
		t.Fatal("advisor.budget_proposals defaults to false, want true")
	}
}

func validBudget() BudgetConfig {
	return BudgetConfig{Name: "app", Match: BudgetMatch{Role: "app"},
		StatementTimeout: "6s"}
}

func TestParseBudgetDuration(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "0": 0, "1500": 1500, "250ms": 250,
		"6s": 6000, "6.5s": 6500, "2min": 120000, "1h": 3600000, " 3 s ": 3000} {
		got, err := ParseBudgetDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseBudgetDuration(%q) = %d, %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"-1s", "6 seconds", "abc", "1.5ms", "6sx"} {
		if got, err := ParseBudgetDuration(in); err == nil {
			t.Errorf("ParseBudgetDuration(%q) = %d, want an error", in, got)
		}
	}
}

// Invalid input: every structural mistake is refused with the budget's
// name and the field in the message.
func TestValidateBudgets_Refusals(t *testing.T) {
	cases := map[string]func(*BudgetConfig){
		"name is required": func(b *BudgetConfig) { b.Name = "" },
		"exactly one of":   func(b *BudgetConfig) { b.Match.ApplicationName = "web" },
		"match must set":   func(b *BudgetConfig) { b.Match.Role = "" },
		"negative":         func(b *BudgetConfig) { b.StatementTimeout = "-6s" },
		"connection_limit": func(b *BudgetConfig) { b.ConnectionLimit = -1 },
		"pool_size":        func(b *BudgetConfig) { b.PoolSize = -2 },
		"mode":             func(b *BudgetConfig) { b.Mode = "block" },
		"sets no limit":    func(b *BudgetConfig) { b.StatementTimeout = "" },
		"warn-only": func(b *BudgetConfig) {
			b.Match = BudgetMatch{ApplicationName: "web"}
			b.Mode = "enforce"
		},
		"key=value":               func(b *BudgetConfig) { b.Match = BudgetMatch{Tag: "route"} },
		"refused":                 func(b *BudgetConfig) { b.Match.Role = "postgres" },
		"provider administration": func(b *BudgetConfig) { b.Match.Role = "rdsadmin" },
		"connection_limit is a role": func(b *BudgetConfig) {
			b.Match = BudgetMatch{ApplicationName: "web"}
			b.ConnectionLimit = 5
		},
	}
	for want, mutate := range cases {
		b := validBudget()
		mutate(&b)
		err := validateBudgets([]BudgetConfig{b})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
	dup := []BudgetConfig{validBudget(), validBudget()}
	if err := validateBudgets(dup); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate names: err = %v", err)
	}
}

// Happy path: role, application and tag budgets in warn and enforce mode.
func TestValidateBudgets_Accepts(t *testing.T) {
	ok := []BudgetConfig{
		validBudget(),
		{Name: "web", Match: BudgetMatch{ApplicationName: "web"}, StatementTimeout: "2s"},
		{Name: "checkout", Match: BudgetMatch{Tag: "route=/checkout"},
			IdleInTransactionSessionTimeout: "30s", Mode: "warn"},
		{Name: "batch", Match: BudgetMatch{Role: "batch"}, ConnectionLimit: 10,
			Mode: "enforce"},
		{Name: "pool", Match: BudgetMatch{Role: "pooled"}, PoolSize: 20},
	}
	if err := validateBudgets(ok); err != nil {
		t.Fatalf("valid budgets refused: %v", err)
	}
	if err := validateBudgets(nil); err != nil {
		t.Fatalf("no budgets refused: %v", err)
	}
}

// The budgets reach Validate: a bad budget fails the whole configuration.
func TestValidate_RefusesABadBudget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Budgets = []BudgetConfig{{Name: "x", Match: BudgetMatch{Role: "app"},
		StatementTimeout: "-1s"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "budgets") {
		t.Fatalf("Validate = %v, want the budget refused", err)
	}
}
