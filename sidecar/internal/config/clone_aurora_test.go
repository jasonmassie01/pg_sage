package config

import (
	"strings"
	"testing"
)

// lab-05c: clone.provider aurora_clone rehearses on an Aurora copy-on-write
// clone of the monitored cluster in clone.aurora.region.

func auroraCloneConfig() *Config {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "aurora_clone"
	cfg.Clone.Aurora.Region = "us-east-1"
	return cfg
}

func TestCloneAuroraDefaults(t *testing.T) {
	a := DefaultConfig().Clone.Aurora
	if a.TTLMinutes != 60 || a.Region != "" || a.InstanceClass != "" {
		t.Fatalf("clone.aurora defaults = %+v", a)
	}
	if DefaultCloneAuroraTTLMinutes != 60 {
		t.Fatalf("default ttl = %d", DefaultCloneAuroraTTLMinutes)
	}
	if err := auroraCloneConfig().validateAgentNative(); err != nil {
		t.Fatalf("aurora_clone with a region and defaults: %v", err)
	}
	if w := auroraCloneConfig().Clone.Warnings(); len(w) != 0 {
		t.Fatalf("warnings = %q", w)
	}
}

// Default value masking: a YAML block naming only the region keeps the
// TTL default instead of zero.
func TestCloneAuroraYAMLKeepsDefaults(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: localhost
  user: postgres
  database: app
clone:
  provider: aurora_clone
  aurora:
    region: eu-west-2
`)
	a := cfg.Clone.Aurora
	if cfg.Clone.Provider != "aurora_clone" || a.Region != "eu-west-2" || a.TTLMinutes != 60 ||
		a.InstanceClass != "" {
		t.Fatalf("loaded clone config = %+v", cfg.Clone)
	}
}

func TestCloneAuroraYAMLReadsEveryKey(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: localhost
  user: postgres
  database: app
clone:
  provider: aurora_clone
  aurora:
    region: us-gov-west-1
    instance_class: db.serverless
    ttl_minutes: 120
`)
	a := cfg.Clone.Aurora
	if a.Region != "us-gov-west-1" || a.InstanceClass != "db.serverless" ||
		a.TTLMinutes != 120 {
		t.Fatalf("clone.aurora = %+v", a)
	}
}

func TestCloneAuroraValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*CloneAuroraConfig)
		want   string
	}{
		"no region":     {func(a *CloneAuroraConfig) { a.Region = "" }, "region is required"},
		"bad region":    {func(a *CloneAuroraConfig) { a.Region = "useast1" }, "AWS region"},
		"bad class":     {func(a *CloneAuroraConfig) { a.InstanceClass = "large" }, "class"},
		"ttl 29":        {func(a *CloneAuroraConfig) { a.TTLMinutes = 29 }, "30-1440"},
		"ttl 1441":      {func(a *CloneAuroraConfig) { a.TTLMinutes = 1441 }, "30-1440"},
		"ttl zero":      {func(a *CloneAuroraConfig) { a.TTLMinutes = 0 }, "30-1440"},
		"ttl negative":  {func(a *CloneAuroraConfig) { a.TTLMinutes = -1 }, "30-1440"},
		"class upper":   {func(a *CloneAuroraConfig) { a.InstanceClass = "db.R6G.large" }, "class"},
		"region spaces": {func(a *CloneAuroraConfig) { a.Region = " us-east-1" }, "AWS region"},
	}
	for name, tc := range cases {
		cfg := auroraCloneConfig()
		tc.mutate(&cfg.Clone.Aurora)
		err := cfg.validateAgentNative()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	for _, ttl := range []int{30, 1440} {
		cfg := auroraCloneConfig()
		cfg.Clone.Aurora.TTLMinutes = ttl
		if err := cfg.validateAgentNative(); err != nil {
			t.Errorf("ttl %d refused: %v", ttl, err)
		}
	}
}

// A leftover clone.aurora block never stops another provider.
func TestCloneAuroraBlockIgnoredForOtherProviders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clone.Aurora.Region = "not a region"
	cfg.Clone.Aurora.TTLMinutes = 0
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("provider none with a broken aurora block: %v", err)
	}
}
