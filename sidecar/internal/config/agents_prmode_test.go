package config

import (
	"strings"
	"testing"
)

// No concurrent access tests: validation reads a value copy.

func TestPRModeDefaultsAreOff(t *testing.T) {
	c := DefaultConfig().Agents.PRMode
	if c.Enabled || c.Repo != "" || c.MigrationsPath != "" {
		t.Fatalf("pr_mode defaults = %+v, want off with no repo or path", c)
	}
	if c.Label != DefaultPRModeLabel || DefaultPRModeLabel != "pg_sage" {
		t.Fatalf("label = %q, want pg_sage", c.Label)
	}
	if err := DefaultConfig().Agents.validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}
}

func TestPRModeValidation(t *testing.T) {
	host := AgentsCodeHostConfig{Provider: "github", Repo: "acme/shop", TimeoutSeconds: 15}
	ok := AgentsPRModeConfig{Enabled: true, MigrationsPath: "db/migrate", Label: "pg_sage"}
	if err := ok.validate(host); err != nil {
		t.Fatalf("valid pr_mode refused: %v", err)
	}
	if got := ok.EffectiveRepo(host); got != "acme/shop" {
		t.Fatalf("EffectiveRepo = %q, want the code host's repo", got)
	}
	other := ok
	other.Repo = "acme/migrations"
	if got := other.EffectiveRepo(host); got != "acme/migrations" {
		t.Fatalf("EffectiveRepo = %q, want the override", got)
	}
	for name, tc := range map[string]struct {
		mutate func(*AgentsPRModeConfig, *AgentsCodeHostConfig)
		want   string
	}{
		"no code host": {func(_ *AgentsPRModeConfig, h *AgentsCodeHostConfig) {
			h.Provider = ""
		}, "agents.code_host.provider"},
		"no path": {func(c *AgentsPRModeConfig, _ *AgentsCodeHostConfig) {
			c.MigrationsPath = ""
		}, "migrations_path is required"},
		"dot dot": {func(c *AgentsPRModeConfig, _ *AgentsCodeHostConfig) {
			c.MigrationsPath = "db/../../etc"
		}, "migrations_path"},
		"absolute": {func(c *AgentsPRModeConfig, _ *AgentsCodeHostConfig) {
			c.MigrationsPath = "/db"
		}, "relative"},
		"bad label": {func(c *AgentsPRModeConfig, _ *AgentsCodeHostConfig) {
			c.Label = "has space"
		}, "label"},
		"bad repo": {func(c *AgentsPRModeConfig, _ *AgentsCodeHostConfig) {
			c.Repo = "acme"
		}, "agents.pr_mode.repo"},
	} {
		c, h := ok, host
		tc.mutate(&c, &h)
		err := c.validate(h)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
	off := AgentsPRModeConfig{MigrationsPath: "../x", Label: "bad label"}
	if err := off.validate(AgentsCodeHostConfig{}); err != nil {
		t.Fatalf("a disabled pr_mode is not validated: %v", err)
	}
}
