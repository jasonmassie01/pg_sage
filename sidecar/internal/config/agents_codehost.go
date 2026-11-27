package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Code-host defaults and bounds (pr-01a).
const (
	DefaultCodeHostTimeoutSeconds = 15
	maxCodeHostTimeoutSeconds     = 120
	// CodeHostTokenEnv holds the code host's read token (also _FILE).
	CodeHostTokenEnv = "SAGE_CODE_HOST_TOKEN"
)

var codeHostRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$`)

// AgentsCodeHostConfig names the code host holding the monitored
// application's repository, which pg_sage reads to find the call site of
// a statement (pr-01a). It is off until a provider is set; the token is
// never in YAML.
type AgentsCodeHostConfig struct {
	Provider       string `yaml:"provider" doc:"Code host of the monitored application's repository: github or gitlab. Empty turns call-site lookups off. The read token comes from SAGE_CODE_HOST_TOKEN (or SAGE_CODE_HOST_TOKEN_FILE). Default: empty."`
	BaseURL        string `yaml:"base_url" doc:"API root of the code host. Empty means https://api.github.com or https://gitlab.com/api/v4; GitHub Enterprise is https://<host>/api/v3. https only, except a loopback relay. Default: empty."`
	Repo           string `yaml:"repo" doc:"Repository of the monitored application's code: owner/name (GitLab: group/sub/name). Required with provider. Default: empty."`
	TimeoutSeconds int    `yaml:"timeout_seconds" doc:"Seconds one code-host request may take. 1 to 120. Default: 15."`
}

func defaultCodeHost() AgentsCodeHostConfig {
	return AgentsCodeHostConfig{TimeoutSeconds: DefaultCodeHostTimeoutSeconds}
}

// Enabled reports whether a code host is configured.
func (c AgentsCodeHostConfig) Enabled() bool { return c.Provider != "" }

func (c AgentsCodeHostConfig) validate() error {
	switch c.Provider {
	case "", "github", "gitlab":
	default:
		return fmt.Errorf("agents.code_host.provider must be github or gitlab (or empty "+
			"for off), got %q", c.Provider)
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > maxCodeHostTimeoutSeconds {
		return fmt.Errorf("agents.code_host.timeout_seconds must be 1 to %d, got %d",
			maxCodeHostTimeoutSeconds, c.TimeoutSeconds)
	}
	if err := c.validateBaseURL(); err != nil {
		return err
	}
	if !c.Enabled() {
		return nil
	}
	return c.validateRepo()
}

func (c AgentsCodeHostConfig) validateRepo() error {
	if c.Repo == "" {
		return fmt.Errorf("agents.code_host.repo is required when agents.code_host.provider " +
			"is set")
	}
	if !codeHostRepoPattern.MatchString(c.Repo) {
		return fmt.Errorf("agents.code_host.repo %q must be owner/name", c.Repo)
	}
	for _, seg := range strings.Split(c.Repo, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("agents.code_host.repo %q has a %q segment", c.Repo, seg)
		}
	}
	if c.Provider == "github" && strings.Count(c.Repo, "/") != 1 {
		return fmt.Errorf("agents.code_host.repo %q must be owner/name on GitHub", c.Repo)
	}
	return nil
}

func (c AgentsCodeHostConfig) validateBaseURL() error {
	if c.BaseURL == "" {
		return nil
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("agents.code_host.base_url is not an absolute http(s) URL")
	}
	if u.User != nil {
		return fmt.Errorf("agents.code_host.base_url must not carry credentials; set " +
			CodeHostTokenEnv)
	}
	return nil
}
