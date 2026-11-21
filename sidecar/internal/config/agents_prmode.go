package config

import (
	"fmt"
	"regexp"
	"strings"
)

// DefaultPRModeLabel labels the pull requests PR mode opens (pr-01b).
const DefaultPRModeLabel = "pg_sage"

var prModeLabelPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,50}$`)

// AgentsPRModeConfig is PR mode (pr-01b): pg_sage opens one pull request
// per migration or exact-literal query rewrite it cannot apply itself, on
// the code host of agents.code_host. Off by default; a person merges.
type AgentsPRModeConfig struct {
	Enabled        bool   `yaml:"enabled" doc:"Open one pull request per migration or exact-literal query rewrite pg_sage cannot apply, on agents.code_host. SAGE_CODE_HOST_TOKEN then needs write access. A person merges. Default: false."`
	Repo           string `yaml:"repo" doc:"Repository the pull requests go to: owner/name (GitLab: group/sub/name). Empty means agents.code_host.repo. Default: empty."`
	MigrationsPath string `yaml:"migrations_path" doc:"Directory of the application's SQL migrations, relative to the repository root. A new migration is named after the newest file there. Required when enabled. Default: empty."`
	Label          string `yaml:"label" doc:"Label on every pull request PR mode opens; the duplicate check lists open pull requests with it. Default: pg_sage."`
}

func defaultPRMode() AgentsPRModeConfig {
	return AgentsPRModeConfig{Label: DefaultPRModeLabel}
}

// EffectiveRepo is the repository PR mode writes to.
func (c AgentsPRModeConfig) EffectiveRepo(host AgentsCodeHostConfig) string {
	if c.Repo != "" {
		return c.Repo
	}
	return host.Repo
}

func (c AgentsPRModeConfig) validate(host AgentsCodeHostConfig) error {
	if !c.Enabled {
		return nil
	}
	if !host.Enabled() {
		return fmt.Errorf("agents.pr_mode.enabled needs agents.code_host.provider " +
			"(github or gitlab) and its repo")
	}
	if c.Repo != "" && !codeHostRepoPattern.MatchString(c.Repo) {
		return fmt.Errorf("agents.pr_mode.repo %q must be owner/name", c.Repo)
	}
	if err := validPRModePath(c.MigrationsPath); err != nil {
		return err
	}
	if !prModeLabelPattern.MatchString(c.Label) {
		return fmt.Errorf("agents.pr_mode.label %q must be 1 to 50 of A-Z a-z 0-9 _ . : -",
			c.Label)
	}
	return nil
}

func validPRModePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("agents.pr_mode.migrations_path is required when enabled")
	case strings.HasPrefix(p, "/"), strings.Contains(p, `\`), len(p) >= 2 && p[1] == ':':
		return fmt.Errorf("agents.pr_mode.migrations_path %q must be relative to the "+
			"repository root", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("agents.pr_mode.migrations_path %q has an empty, \".\" or "+
				"\"..\" segment", p)
		}
	}
	return nil
}
