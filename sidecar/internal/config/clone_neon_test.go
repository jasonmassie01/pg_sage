package config

import (
	"strings"
	"testing"
)

// lab-05b: clone.provider neon_branch rehearses on a Neon branch of
// clone.neon.project_id.

func neonCloneConfig() *Config {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "neon_branch"
	cfg.Clone.Neon.ProjectID = "shiny-wind-028834"
	cfg.Clone.Neon.APIKey = "napi_test_key_not_real"
	return cfg
}

func TestCloneNeonDefaults(t *testing.T) {
	n := DefaultConfig().Clone.Neon
	if n.TTLMinutes != 60 || n.EndpointMaxCU != 1 || n.ProjectID != "" ||
		n.ParentBranch != "" || n.APIKey != "" {
		t.Fatalf("clone.neon defaults = %+v", n)
	}
	if DefaultCloneNeonTTLMinutes != 60 || DefaultCloneNeonEndpointMaxCU != 1 {
		t.Fatalf("default constants %d, %v", DefaultCloneNeonTTLMinutes,
			DefaultCloneNeonEndpointMaxCU)
	}
	if err := neonCloneConfig().validateAgentNative(); err != nil {
		t.Fatalf("neon_branch with a project, a key and defaults: %v", err)
	}
}

// Default value masking: a YAML block naming only the project keeps the
// TTL and size defaults instead of zero.
func TestCloneNeonYAMLKeepsDefaults(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: localhost
  user: postgres
  database: app
clone:
  provider: neon_branch
  neon:
    project_id: shiny-wind-028834
`)
	n := cfg.Clone.Neon
	if cfg.Clone.Provider != "neon_branch" || n.ProjectID != "shiny-wind-028834" ||
		n.TTLMinutes != 60 || n.EndpointMaxCU != 1 {
		t.Fatalf("loaded clone config = %+v", cfg.Clone)
	}
}

func TestCloneNeonYAMLReadsEveryKey(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: localhost
  user: postgres
  database: app
clone:
  provider: neon_branch
  neon:
    project_id: shiny-wind-028834
    parent_branch: br-aged-salad-637688
    api_key: napi_from_yaml
    endpoint_max_cu: 2
    ttl_minutes: 90
`)
	want := CloneNeonConfig{ProjectID: "shiny-wind-028834",
		ParentBranch: "br-aged-salad-637688", APIKey: "napi_from_yaml", EndpointMaxCU: 2,
		TTLMinutes: 90}
	if cfg.Clone.Neon != want {
		t.Fatalf("clone.neon = %+v, want %+v", cfg.Clone.Neon, want)
	}
}

func TestCloneNeonInvalid(t *testing.T) {
	type invalid struct {
		mutate func(*CloneNeonConfig)
		want   string
	}
	cases := map[string]invalid{
		"no project": {func(n *CloneNeonConfig) { n.ProjectID = "" },
			"project_id is required"},
		"uppercase project": {func(n *CloneNeonConfig) { n.ProjectID = "Shiny-Wind" },
			"project_id"},
		"slash project": {func(n *CloneNeonConfig) { n.ProjectID = "a/b" }, "project_id"},
		"ttl zero":      {func(n *CloneNeonConfig) { n.TTLMinutes = 0 }, "ttl_minutes"},
		"ttl above max": {func(n *CloneNeonConfig) { n.TTLMinutes = 1441 }, "ttl_minutes"},
		"ttl negative":  {func(n *CloneNeonConfig) { n.TTLMinutes = -1 }, "ttl_minutes"},
		"cu zero":       {func(n *CloneNeonConfig) { n.EndpointMaxCU = 0 }, "endpoint_max_cu"},
		"cu below min": {func(n *CloneNeonConfig) { n.EndpointMaxCU = 0.1 },
			"endpoint_max_cu"},
		"cu above max": {func(n *CloneNeonConfig) { n.EndpointMaxCU = 8.5 },
			"endpoint_max_cu"},
		"parent with space": {func(n *CloneNeonConfig) { n.ParentBranch = "my dev" },
			"parent_branch"},
		"parent too long": {func(n *CloneNeonConfig) {
			n.ParentBranch = strings.Repeat("a", 257)
		}, "parent_branch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := neonCloneConfig()
			tc.mutate(&cfg.Clone.Neon)
			err := cfg.validateAgentNative()
			if err == nil || !strings.Contains(err.Error(), "clone.neon."+tc.want) {
				t.Fatalf("err = %v, want it to name clone.neon.%s", err, tc.want)
			}
		})
	}
}

func TestCloneNeonBoundariesAreAccepted(t *testing.T) {
	for _, tc := range []struct {
		ttl    int
		cu     float64
		parent string
	}{{1, 0.25, ""}, {CloneNeonMaxTTLMinutes, 8, "dev"}, {60, 1, "br-aged-salad-637688"},
		{60, 1, "feature/login.v2"}} {
		cfg := neonCloneConfig()
		cfg.Clone.Neon.TTLMinutes, cfg.Clone.Neon.EndpointMaxCU = tc.ttl, tc.cu
		cfg.Clone.Neon.ParentBranch = tc.parent
		if err := cfg.validateAgentNative(); err != nil {
			t.Fatalf("ttl %d cu %v parent %q refused: %v", tc.ttl, tc.cu, tc.parent, err)
		}
	}
}

// No API key is valid: the provider is unavailable and rehearsals are
// recommend-only, and the warning says why.
func TestCloneNeonWithoutAKeyWarns(t *testing.T) {
	cfg := neonCloneConfig()
	cfg.Clone.Neon.APIKey = ""
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("neon_branch without a key must load: %v", err)
	}
	w := cfg.Clone.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "clone.neon.api_key") ||
		!strings.Contains(w[0], "recommend-only") {
		t.Fatalf("warnings = %v, want one naming clone.neon.api_key", w)
	}
	if w := neonCloneConfig().Clone.Warnings(); len(w) != 0 {
		t.Fatalf("warnings with a key = %v", w)
	}
}

func TestCloneNeonIgnoredForOtherProviders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "none"
	cfg.Clone.Neon.ProjectID = "Not-Valid"
	cfg.Clone.Neon.TTLMinutes = 0
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("a leftover clone.neon block stopped provider none: %v", err)
	}
	if w := cfg.Clone.Warnings(); len(w) != 0 {
		t.Fatalf("warnings for provider none = %v", w)
	}
}

func TestCloneNeonAPIKeyFromEnvAndFile(t *testing.T) {
	chdirTemp(t)
	clearSecretEnv(t)
	t.Setenv("SAGE_CLONE_NEON_API_KEY", "napi_from_env")
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Clone.Neon.APIKey != "napi_from_env" {
		t.Fatalf("api key from env = %q", cfg.Clone.Neon.APIKey)
	}
	t.Setenv("SAGE_CLONE_NEON_API_KEY", "")
	t.Setenv("SAGE_CLONE_NEON_API_KEY_FILE", writeSecretFile(t, "napi_from_file\n", 0o600))
	cfg, err = Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Clone.Neon.APIKey != "napi_from_file" {
		t.Fatalf("api key from file = %q", cfg.Clone.Neon.APIKey)
	}
}

func TestCloneNeonAPIKeyIsASafetyCriticalSecret(t *testing.T) {
	cfg := neonCloneConfig()
	doc, ok := DescribeField(cfg, "clone.neon.api_key")
	if !ok || !doc.Secret || doc.HasValue || doc.Value != nil {
		t.Fatalf("clone.neon.api_key doc = %+v (found %v), want a secret without value", doc, ok)
	}
	for _, k := range []string{"clone.neon.api_key", "clone.neon.project_id",
		"clone.neon.parent_branch"} {
		if c, ok := KeyClassOf(k); !ok || c != KeySafetyCritical {
			t.Fatalf("%s class = %q (%v), want safety_critical", k, c, ok)
		}
	}
	for _, k := range []string{"clone.neon.ttl_minutes", "clone.neon.endpoint_max_cu"} {
		if c, ok := KeyClassOf(k); !ok || c != KeyOperatorPreference {
			t.Fatalf("%s class = %q (%v), want operator_preference", k, c, ok)
		}
	}
}

func TestCloneProviderNeonBranchInTheEnum(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "neon_branch"
	err := cfg.validateAgentNative()
	if err == nil || !strings.Contains(err.Error(), "clone.neon.project_id is required") {
		t.Fatalf("neon_branch without a project: %v", err)
	}
}
