package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests: callSiteSource runs once at startup.

func codeHostConfig(provider, repo, base string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Agents.CodeHost = config.AgentsCodeHostConfig{Provider: provider, Repo: repo,
		BaseURL: base, TimeoutSeconds: 7}
	return cfg
}

func TestCallSiteSourceOffByDefault(t *testing.T) {
	src, err := callSiteSource(config.DefaultConfig())
	if err != nil || src != nil {
		t.Fatalf("default config: source %+v, err %v; want nil, nil", src, err)
	}
	src, err = callSiteSource(nil)
	if err != nil || src != nil {
		t.Fatalf("nil config: source %+v, err %v; want nil, nil", src, err)
	}
}

func TestCallSiteSourceBuildsEachProvider(t *testing.T) {
	t.Setenv(config.CodeHostTokenEnv, "tok-for-test")
	gh, err := callSiteSource(codeHostConfig("github", "acme/shop", ""))
	if err != nil {
		t.Fatalf("github: %v", err)
	}
	if _, ok := gh.Host.(*codehost.GitHub); !ok || gh.Repo != "acme/shop" ||
		gh.Host.Name() != "github api.github.com" {
		t.Fatalf("github source = %+v (%s)", gh, gh.Host.Name())
	}
	gl, err := callSiteSource(codeHostConfig("gitlab", "acme/backend/shop",
		"https://gitlab.example.com/api/v4"))
	if err != nil {
		t.Fatalf("gitlab: %v", err)
	}
	if _, ok := gl.Host.(*codehost.GitLab); !ok ||
		gl.Host.Name() != "gitlab gitlab.example.com" {
		t.Fatalf("gitlab source = %+v", gl)
	}
}

func TestCallSiteSourceFailsClosedOnBadURLAndUnreadableTokenFile(t *testing.T) {
	_, err := callSiteSource(codeHostConfig("github", "acme/shop",
		"http://github.example.com"))
	if !errors.Is(err, codehost.ErrInvalid) ||
		!strings.Contains(err.Error(), "agents.code_host") {
		t.Fatalf("plain-http base URL: err = %v, want a refusal naming agents.code_host", err)
	}
	t.Setenv(config.CodeHostTokenEnv+"_FILE", filepath.Join(t.TempDir(), "missing"))
	_, err = callSiteSource(codeHostConfig("github", "acme/shop", ""))
	if err == nil || !strings.Contains(err.Error(), config.CodeHostTokenEnv) {
		t.Fatalf("unreadable token file: err = %v, want it to name %s", err,
			config.CodeHostTokenEnv)
	}
}
