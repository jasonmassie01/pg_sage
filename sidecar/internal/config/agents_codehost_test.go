package config

import (
	"strings"
	"testing"
)

// No concurrent access or state transition tests: the code-host settings
// are plain values checked once at load.

func TestCodeHostDefaultsAreOff(t *testing.T) {
	c := DefaultConfig().Agents.CodeHost
	if c.Provider != "" || c.Repo != "" || c.BaseURL != "" {
		t.Fatalf("code host must be off by default, got %+v", c)
	}
	if c.TimeoutSeconds != 15 {
		t.Fatalf("timeout_seconds default = %d, want 15", c.TimeoutSeconds)
	}
	if c.Enabled() {
		t.Fatal("Enabled() with no provider")
	}
	if err := c.validate(); err != nil {
		t.Fatalf("default code host config invalid: %v", err)
	}
}

func TestCodeHostValidation(t *testing.T) {
	ok := AgentsCodeHostConfig{Provider: "github", Repo: "acme/shop", TimeoutSeconds: 15}
	cases := map[string]struct {
		mutate func(*AgentsCodeHostConfig)
		want   string
	}{
		"unknown provider": {func(c *AgentsCodeHostConfig) { c.Provider = "bitbucket" },
			"agents.code_host.provider must be github or gitlab"},
		"no repo": {func(c *AgentsCodeHostConfig) { c.Repo = "" },
			"agents.code_host.repo is required"},
		"repo without owner": {func(c *AgentsCodeHostConfig) { c.Repo = "shop" },
			"agents.code_host.repo"},
		"repo dot dot": {func(c *AgentsCodeHostConfig) { c.Repo = "acme/../x" },
			"agents.code_host.repo"},
		"zero timeout": {func(c *AgentsCodeHostConfig) { c.TimeoutSeconds = 0 },
			"agents.code_host.timeout_seconds must be 1 to 120"},
		"long timeout": {func(c *AgentsCodeHostConfig) { c.TimeoutSeconds = 121 },
			"agents.code_host.timeout_seconds must be 1 to 120"},
		"relative base": {func(c *AgentsCodeHostConfig) { c.BaseURL = "github.example.com" },
			"agents.code_host.base_url"},
		"base with token": {func(c *AgentsCodeHostConfig) {
			c.BaseURL = "https://x:tok@github.example.com"
		}, "agents.code_host.base_url must not carry credentials"},
	}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	for name, tc := range cases {
		c := ok
		tc.mutate(&c)
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
	gl := AgentsCodeHostConfig{Provider: "gitlab", Repo: "acme/backend/shop",
		BaseURL: "https://gitlab.example.com/api/v4", TimeoutSeconds: 1}
	if err := gl.validate(); err != nil || !gl.Enabled() {
		t.Fatalf("nested GitLab project refused: %v", err)
	}
	gh := AgentsCodeHostConfig{Provider: "github", Repo: "acme/backend/shop",
		TimeoutSeconds: 15}
	if err := gh.validate(); err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("GitHub repo with a subgroup: err = %v", err)
	}
}

func TestCodeHostValidationRunsFromAgentsValidate(t *testing.T) {
	a := defaultAgentsConfig()
	a.CodeHost.Provider = "github"
	err := a.validate()
	if err == nil || !strings.Contains(err.Error(), "agents.code_host.repo is required") {
		t.Fatalf("agents.validate did not check code_host: %v", err)
	}
}
