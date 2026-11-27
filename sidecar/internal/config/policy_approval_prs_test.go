package config

import (
	"strings"
	"testing"
	"time"
)

// cli-02b: policy.approval_prs. No concurrent access or state transition
// tests: the settings are plain values checked once at load.

func TestApprovalPRsDefaultsAreOff(t *testing.T) {
	c := DefaultConfig().Policy.ApprovalPRs
	if c.Enabled() || c.Repo != "" {
		t.Fatalf("approval PRs must be off by default, got %+v", c)
	}
	if c.Provider != "github" || c.Path != "approvals/" || c.ExpiryHours != 72 ||
		c.TimeoutSeconds != 15 {
		t.Fatalf("defaults = %+v, want github, approvals/, 72 h, 15 s", c)
	}
	if c.Expiry() != 72*time.Hour {
		t.Fatalf("Expiry() = %s, want 72h", c.Expiry())
	}
	if err := DefaultConfig().Policy.validate(); err != nil {
		t.Fatalf("default policy config invalid: %v", err)
	}
}

func TestApprovalPRsZeroValuesFallBackToDefaults(t *testing.T) {
	var c PolicyApprovalPRsConfig
	if c.Expiry() != 72*time.Hour || c.EffectivePath() != "approvals/" {
		t.Fatalf("zero config: expiry %s, path %q; want the defaults", c.Expiry(),
			c.EffectivePath())
	}
	c.Path = "reviews/pg"
	if c.EffectivePath() != "reviews/pg/" {
		t.Fatalf("path without a slash = %q, want reviews/pg/", c.EffectivePath())
	}
}

func TestApprovalPRsValidation(t *testing.T) {
	ok := PolicyConfig{Profile: "staffed", Directory: "gitops",
		ApprovalPRs: PolicyApprovalPRsConfig{Provider: "github", Repo: "acme/config",
			Path: "approvals/", ExpiryHours: 72, TimeoutSeconds: 15}}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	cases := map[string]struct {
		mutate func(*PolicyConfig)
		want   string
	}{
		"no policy.directory": {func(c *PolicyConfig) { c.Directory = "" },
			"policy.approval_prs.repo needs policy.directory"},
		"unknown provider": {func(c *PolicyConfig) { c.ApprovalPRs.Provider = "gitea" },
			"policy.approval_prs.provider must be github or gitlab"},
		"repo without owner": {func(c *PolicyConfig) { c.ApprovalPRs.Repo = "config" },
			"policy.approval_prs.repo"},
		"github nested repo": {func(c *PolicyConfig) { c.ApprovalPRs.Repo = "a/b/c" },
			"owner/name on GitHub"},
		"absolute path": {func(c *PolicyConfig) { c.ApprovalPRs.Path = "/approvals" },
			"policy.approval_prs.path"},
		"dot dot path": {func(c *PolicyConfig) { c.ApprovalPRs.Path = "a/../b" },
			"policy.approval_prs.path"},
		"bad branch": {func(c *PolicyConfig) { c.ApprovalPRs.BaseBranch = "main..x" },
			"policy.approval_prs.base_branch"},
		"expiry zero": {func(c *PolicyConfig) { c.ApprovalPRs.ExpiryHours = 0 },
			"policy.approval_prs.expiry_hours must be 1 to 720"},
		"expiry too long": {func(c *PolicyConfig) { c.ApprovalPRs.ExpiryHours = 721 },
			"policy.approval_prs.expiry_hours must be 1 to 720"},
		"timeout zero": {func(c *PolicyConfig) { c.ApprovalPRs.TimeoutSeconds = 0 },
			"policy.approval_prs.timeout_seconds"},
		"credentials in base_url": {func(c *PolicyConfig) {
			c.ApprovalPRs.BaseURL = "https://u:p@ghe.example.com/api/v3"
		}, "must not carry credentials"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := ok
			tc.mutate(&c)
			err := c.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestApprovalPRsBoundariesAccepted(t *testing.T) {
	for _, hours := range []int{1, 720} {
		c := PolicyConfig{Profile: "staffed", Directory: "d",
			ApprovalPRs: PolicyApprovalPRsConfig{Provider: "gitlab", Repo: "g/sub/cfg",
				ExpiryHours: hours, TimeoutSeconds: 120}}
		if err := c.validate(); err != nil {
			t.Fatalf("expiry_hours %d refused: %v", hours, err)
		}
	}
}

func TestApprovalPRsTokenEnvIsRegisteredAndFileCapable(t *testing.T) {
	for _, e := range envRegistry {
		if e.Name == ApprovalPRsTokenEnv {
			if !e.File {
				t.Fatalf("%s must accept a _FILE variant", ApprovalPRsTokenEnv)
			}
			return
		}
	}
	t.Fatalf("%s is not in the environment registry", ApprovalPRsTokenEnv)
}
