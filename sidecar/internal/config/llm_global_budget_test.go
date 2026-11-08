package config

import (
	"os"
	"path/filepath"
	"testing"
)

// llm-03: llm.global_token_budget_daily defaults to 0 ("derived": the
// fleet budget when set, else the sum of every client's budget) and is
// hot-reloadable through the "llm" owner.

func TestGlobalTokenBudgetDefaultsToDerived(t *testing.T) {
	if got := DefaultConfig().LLM.GlobalTokenBudgetDaily; got != 0 {
		t.Fatalf("default llm.global_token_budget_daily = %d, want 0 (derived)", got)
	}
}

func TestGlobalTokenBudgetLoadsFromYAML(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "llm:\n  global_token_budget_daily: 750000\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.LLM.GlobalTokenBudgetDaily != 750000 {
		t.Fatalf("global_token_budget_daily = %d, want 750000",
			cfg.LLM.GlobalTokenBudgetDaily)
	}
	if cfg.LLM.TokenBudgetDaily != DefaultLLMTokenBudget {
		t.Fatalf("token_budget_daily = %d, want the default kept", cfg.LLM.TokenBudgetDaily)
	}
}

func TestGlobalTokenBudgetIsHotReloadable(t *testing.T) {
	lifecycle, ok := LookupFieldLifecycle("llm.global_token_budget_daily")
	if !ok {
		t.Fatal("llm.global_token_budget_daily has no lifecycle")
	}
	if lifecycle.Lifecycle != LifecycleReconfigure || lifecycle.Owner != "llm" {
		t.Fatalf("lifecycle = %+v, want reconfigure by llm", lifecycle)
	}
}
