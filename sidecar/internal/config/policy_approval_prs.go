package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Approval PR defaults and bounds (cli-02b).
const (
	DefaultApprovalPRsPath        = "approvals/"
	DefaultApprovalPRsExpiryHours = 72
	maxApprovalPRsExpiryHours     = 720
	// ApprovalPRsTokenEnv holds the config repository's write token (also
	// _FILE); it opens, comments on and closes approval PRs.
	ApprovalPRsTokenEnv = "SAGE_APPROVAL_PRS_TOKEN"
)

var approvalPRsBranchPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// PolicyApprovalPRsConfig opens each change that waits for approval as a
// pull request on a config repository (cli-02b). Off until repo is set; a
// merge by a mapped operator approves the change; the token is never in
// YAML.
type PolicyApprovalPRsConfig struct {
	Provider       string `yaml:"provider" doc:"Code host of the config repository: github or gitlab. Default: github."`
	BaseURL        string `yaml:"base_url" doc:"API root of the code host. Empty means https://api.github.com or https://gitlab.com/api/v4. https only, except a loopback relay. Default: empty."`
	Repo           string `yaml:"repo" doc:"Config repository approval PRs open on: owner/name (GitLab: group/sub/name). Empty: off. Needs policy.directory (principals/users.yaml). Token: SAGE_APPROVAL_PRS_TOKEN. Default: empty." warning:"A merge of an approval PR by a mapped operator approves and runs that change. High-risk changes, and tiers above notification_policy.chat_approval_max_tier, are never opened as PRs."`
	BaseBranch     string `yaml:"base_branch" doc:"Branch approval PRs merge into. Empty means the repository's default branch. Default: empty."`
	Path           string `yaml:"path" doc:"Directory of the repository the approval files are added under, as <path><database>/<queue_id>.yaml. Default: approvals/."`
	ExpiryHours    int    `yaml:"expiry_hours" doc:"Hours an approval PR stays open before pg_sage closes it as expired (sooner when the change itself expires), 1 to 720. Default: 72."`
	TimeoutSeconds int    `yaml:"timeout_seconds" doc:"Seconds one code-host request may take. 1 to 120. Default: 15."`
}

func defaultApprovalPRs() PolicyApprovalPRsConfig {
	return PolicyApprovalPRsConfig{Provider: "github", Path: DefaultApprovalPRsPath,
		ExpiryHours: DefaultApprovalPRsExpiryHours, TimeoutSeconds: DefaultCodeHostTimeoutSeconds}
}

// Enabled reports whether approval PRs are configured.
func (c PolicyApprovalPRsConfig) Enabled() bool { return strings.TrimSpace(c.Repo) != "" }

// Expiry is expiry_hours as a duration; zero keeps the default.
func (c PolicyApprovalPRsConfig) Expiry() time.Duration {
	if c.ExpiryHours <= 0 {
		return DefaultApprovalPRsExpiryHours * time.Hour
	}
	return time.Duration(c.ExpiryHours) * time.Hour
}

// EffectivePath is path with a trailing slash; empty is the default.
func (c PolicyApprovalPRsConfig) EffectivePath() string {
	p := strings.TrimSpace(c.Path)
	if p == "" {
		return DefaultApprovalPRsPath
	}
	return strings.TrimSuffix(p, "/") + "/"
}

// validate checks the settings; directory is policy.directory.
func (c PolicyApprovalPRsConfig) validate(directory string) error {
	if !c.Enabled() {
		return nil
	}
	if strings.TrimSpace(directory) == "" {
		return fmt.Errorf("policy.approval_prs.repo needs policy.directory: its " +
			"principals/users.yaml maps code-host logins to pg_sage users")
	}
	host := AgentsCodeHostConfig{Provider: c.Provider, BaseURL: c.BaseURL, Repo: c.Repo,
		TimeoutSeconds: c.TimeoutSeconds}
	if err := host.validate(); err != nil {
		return fmt.Errorf("policy.approval_prs: %s", strings.ReplaceAll(err.Error(),
			"agents.code_host.", "policy.approval_prs."))
	}
	if c.Provider == "" {
		return fmt.Errorf("policy.approval_prs.provider must be github or gitlab")
	}
	if err := c.validatePath(); err != nil {
		return err
	}
	if b := c.BaseBranch; b != "" && (!approvalPRsBranchPattern.MatchString(b) ||
		strings.Contains(b, "..") || strings.HasPrefix(b, "-") ||
		strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/")) {
		return fmt.Errorf("policy.approval_prs.base_branch %q is not a branch name", b)
	}
	if c.ExpiryHours < 1 || c.ExpiryHours > maxApprovalPRsExpiryHours {
		return fmt.Errorf("policy.approval_prs.expiry_hours must be 1 to %d, got %d",
			maxApprovalPRsExpiryHours, c.ExpiryHours)
	}
	return nil
}

// validatePath requires a relative path of plain segments.
func (c PolicyApprovalPRsConfig) validatePath() error {
	p := strings.TrimSuffix(c.EffectivePath(), "/")
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return fmt.Errorf("policy.approval_prs.path %q must be relative to the repository "+
			"root", c.Path)
	}
	if _, err := url.Parse(p); err != nil {
		return fmt.Errorf("policy.approval_prs.path %q is not a path: %v", c.Path, err)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("policy.approval_prs.path %q has an empty, \".\" or \"..\" "+
				"segment", c.Path)
		}
	}
	return nil
}
