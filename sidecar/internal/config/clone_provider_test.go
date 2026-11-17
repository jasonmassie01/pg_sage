package config

import (
	"strings"
	"testing"
)

// market-03: clone.provider lakebase_branch rehearses on an instant
// Lakebase branch of clone.lakebase.project.

func lakebaseCloneConfig() *Config {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "lakebase_branch"
	cfg.Clone.Lakebase.Project = "shop"
	return cfg
}

func TestCloneLakebaseDefaults(t *testing.T) {
	lb := DefaultConfig().Clone.Lakebase
	if lb.TTLMinutes != 60 || lb.EndpointMaxCU != DefaultCloneLakebaseEndpointMaxCU ||
		lb.Project != "" || lb.SourceBranch != "" {
		t.Fatalf("clone.lakebase defaults = %+v", lb)
	}
	if DefaultCloneLakebaseEndpointMaxCU <= 0 {
		t.Fatalf("default endpoint_max_cu %v is not positive", DefaultCloneLakebaseEndpointMaxCU)
	}
	if err := lakebaseCloneConfig().validateAgentNative(); err != nil {
		t.Fatalf("lakebase_branch with a project and defaults: %v", err)
	}
}

// Default value masking: a YAML block naming only the project keeps the
// TTL and size defaults instead of zero.
func TestCloneLakebaseYAMLKeepsDefaults(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: localhost
  user: postgres
  database: app
clone:
  provider: lakebase_branch
  lakebase:
    project: shop
`)
	lb := cfg.Clone.Lakebase
	if cfg.Clone.Provider != "lakebase_branch" || lb.Project != "shop" ||
		lb.TTLMinutes != 60 || lb.EndpointMaxCU != DefaultCloneLakebaseEndpointMaxCU {
		t.Fatalf("loaded clone config = %+v", cfg.Clone)
	}
}

func TestCloneLakebaseYAMLReadsEveryKey(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: localhost
  user: postgres
  database: app
clone:
  provider: lakebase_branch
  lakebase:
    project: shop
    source_branch: staging
    ttl_minutes: 90
    endpoint_max_cu: 4
`)
	lb := cfg.Clone.Lakebase
	if lb.SourceBranch != "staging" || lb.TTLMinutes != 90 || lb.EndpointMaxCU != 4 {
		t.Fatalf("loaded clone.lakebase = %+v", lb)
	}
}

func TestCloneLakebaseInvalid(t *testing.T) {
	cases := map[string]struct {
		mutate func(*CloneLakebaseConfig)
		want   string
	}{
		"no project": {func(c *CloneLakebaseConfig) { c.Project = "" },
			"clone.lakebase.project is required"},
		"uppercase project": {func(c *CloneLakebaseConfig) { c.Project = "Shop" },
			"clone.lakebase.project"},
		"ttl zero": {func(c *CloneLakebaseConfig) { c.TTLMinutes = 0 },
			"clone.lakebase.ttl_minutes must be within 1-1440"},
		"ttl over a day": {func(c *CloneLakebaseConfig) { c.TTLMinutes = 1441 },
			"clone.lakebase.ttl_minutes must be within 1-1440"},
		"ttl negative": {func(c *CloneLakebaseConfig) { c.TTLMinutes = -5 },
			"clone.lakebase.ttl_minutes"},
		"max cu zero": {func(c *CloneLakebaseConfig) { c.EndpointMaxCU = 0 },
			"clone.lakebase.endpoint_max_cu must be positive"},
		"source in another project": {func(c *CloneLakebaseConfig) {
			c.SourceBranch = "projects/other/branches/main"
		}, "clone.lakebase.source_branch"},
		"uppercase source": {func(c *CloneLakebaseConfig) { c.SourceBranch = "Main" },
			"clone.lakebase.source_branch"},
		"endpoint as source": {func(c *CloneLakebaseConfig) {
			c.SourceBranch = "projects/shop/branches/main/endpoints/e"
		}, "clone.lakebase.source_branch"},
	}
	for name, tc := range cases {
		cfg := lakebaseCloneConfig()
		tc.mutate(&cfg.Clone.Lakebase)
		err := cfg.validateAgentNative()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestCloneLakebaseBoundariesAndSources(t *testing.T) {
	for _, ttl := range []int{1, 1440} {
		cfg := lakebaseCloneConfig()
		cfg.Clone.Lakebase.TTLMinutes = ttl
		if err := cfg.validateAgentNative(); err != nil {
			t.Errorf("ttl_minutes %d refused: %v", ttl, err)
		}
	}
	for _, source := range []string{"", "staging", "projects/shop/branches/stage-2"} {
		cfg := lakebaseCloneConfig()
		cfg.Clone.Lakebase.SourceBranch = source
		if err := cfg.validateAgentNative(); err != nil {
			t.Errorf("source_branch %q refused: %v", source, err)
		}
	}
}

// The lakebase block is checked only when it is the provider in use, so a
// leftover block never stops a DLE deployment.
func TestCloneLakebaseIgnoredForOtherProviders(t *testing.T) {
	for _, provider := range []string{"none", "dle"} {
		cfg := DefaultConfig()
		cfg.Clone.Provider = provider
		cfg.Clone.DLEEndpoint = "https://dle.invalid"
		cfg.Clone.Lakebase.TTLMinutes = 0
		if err := cfg.validateAgentNative(); err != nil {
			t.Errorf("%s with an unused lakebase block: %v", provider, err)
		}
	}
}

func TestCloneProviderUnknownNamesEveryAdapter(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "Lakebase_Branch"
	err := cfg.validateAgentNative()
	if err == nil || !strings.Contains(err.Error(), "lakebase_branch") ||
		!strings.Contains(err.Error(), "dle") {
		t.Fatalf("unknown provider = %v", err)
	}
}

// lab-05a: clone.provider snapshot was replaced by the full-copy provider
// names, a full copy needs clone.allow_full_copy, and every deployment
// makes at most clone.max_creates_per_hour clones.

func TestCloneProviderSnapshotNamesItsReplacements(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "snapshot"
	err := cfg.validateAgentNative()
	if err == nil {
		t.Fatal("clone.provider snapshot was accepted")
	}
	for _, name := range CloneFullCopyProviders {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the snapshot refusal %q does not name %s", err, name)
		}
	}
	if len(CloneFullCopyProviders) != 4 {
		t.Fatalf("full-copy providers = %v, want rds_restore, cloudsql_clone, "+
			"alloydb_restore and azure_restore", CloneFullCopyProviders)
	}
}

func TestCloneFullCopyProvidersHaveNoAdapterYet(t *testing.T) {
	for _, name := range CloneFullCopyProviders {
		cfg := DefaultConfig()
		cfg.Clone.Provider = name
		cfg.Clone.AllowFullCopy = true
		err := cfg.validateAgentNative()
		if err == nil || !strings.Contains(err.Error(), name) ||
			!strings.Contains(err.Error(), "not in this build") {
			t.Errorf("clone.provider %s: %v", name, err)
		}
	}
}

func TestCloneAllowFullCopyWithoutAFullCopyProviderWarns(t *testing.T) {
	for _, provider := range []string{"none", "dle"} {
		cfg := DefaultConfig()
		cfg.Clone.Provider = provider
		cfg.Clone.DLEEndpoint = "https://dle.invalid"
		cfg.Clone.AllowFullCopy = true
		if err := cfg.validateAgentNative(); err != nil {
			t.Fatalf("%s with allow_full_copy is an error, want a warning: %v", provider, err)
		}
		w := cfg.Clone.Warnings()
		if len(w) != 1 || !strings.Contains(w[0], "clone.allow_full_copy") ||
			!strings.Contains(w[0], provider) {
			t.Fatalf("%s warnings = %v", provider, w)
		}
	}
	cfg := DefaultConfig()
	if w := cfg.Clone.Warnings(); len(w) != 0 {
		t.Fatalf("defaults warn: %v", w)
	}
}

func TestCloneLifecycleDefaults(t *testing.T) {
	c := DefaultConfig().Clone
	if c.AllowFullCopy || c.MaxCreatesPerHour != 4 || DefaultCloneMaxCreatesPerHour != 4 {
		t.Fatalf("defaults allow_full_copy %v, max_creates_per_hour %d; want false, 4",
			c.AllowFullCopy, c.MaxCreatesPerHour)
	}
	cfg := loadFromYAML(t, `
postgres:
  host: localhost
  user: postgres
  database: app
clone:
  provider: dle
  dle_endpoint: https://dle.invalid
`)
	if cfg.Clone.MaxCreatesPerHour != 4 || cfg.Clone.AllowFullCopy {
		t.Fatalf("a clone block without the keys loaded %+v", cfg.Clone)
	}
	cfg = loadFromYAML(t, `
postgres:
  host: localhost
  user: postgres
  database: app
clone:
  provider: dle
  dle_endpoint: https://dle.invalid
  allow_full_copy: true
  max_creates_per_hour: 9
`)
	if cfg.Clone.MaxCreatesPerHour != 9 || !cfg.Clone.AllowFullCopy {
		t.Fatalf("explicit keys loaded %+v", cfg.Clone)
	}
}

func TestCloneMaxCreatesPerHourBounds(t *testing.T) {
	for n, ok := range map[int]bool{-1: false, 0: true, 1: true, 60: true, 61: false} {
		cfg := DefaultConfig()
		cfg.Clone.MaxCreatesPerHour = n
		err := cfg.validateAgentNative()
		if (err == nil) != ok {
			t.Errorf("max_creates_per_hour %d: %v", n, err)
		}
		if err != nil && !strings.Contains(err.Error(), "clone.max_creates_per_hour") {
			t.Errorf("max_creates_per_hour %d error does not name the key: %v", n, err)
		}
	}
}
