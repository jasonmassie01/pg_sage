package main

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/config"
)

// callSiteSource builds the code host the query page reads call sites
// from (pr-01a). nil means agents.code_host is off. A configured host
// that cannot be built fails startup: an operator who set it expects it.
func callSiteSource(cfg *config.Config) (*api.CallSiteSource, error) {
	host, err := codeHostClient(cfg)
	if err != nil || host == nil {
		return nil, err
	}
	return &api.CallSiteSource{Host: host, Repo: cfg.Agents.CodeHost.Repo}, nil
}

// codeHostClient builds the configured code host; nil when it is off.
func codeHostClient(cfg *config.Config) (codehost.Host, error) {
	if cfg == nil || !cfg.Agents.CodeHost.Enabled() {
		return nil, nil
	}
	ch := cfg.Agents.CodeHost
	getenv, err := secretGetenv(config.CodeHostTokenEnv)
	if err != nil {
		return nil, fmt.Errorf("agents.code_host: read %s: %w", config.CodeHostTokenEnv, err)
	}
	opts := codehost.Options{BaseURL: ch.BaseURL, Token: getenv(config.CodeHostTokenEnv),
		Timeout: time.Duration(ch.TimeoutSeconds) * time.Second}
	switch ch.Provider {
	case "github":
		gh, err := codehost.NewGitHub(opts)
		if err != nil {
			return nil, fmt.Errorf("agents.code_host: %w", err)
		}
		return gh, nil
	case "gitlab":
		gl, err := codehost.NewGitLab(opts)
		if err != nil {
			return nil, fmt.Errorf("agents.code_host: %w", err)
		}
		return gl, nil
	}
	return nil, fmt.Errorf("agents.code_host.provider %q is not github or gitlab", ch.Provider)
}
