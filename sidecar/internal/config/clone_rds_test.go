package config

import (
	"strings"
	"testing"
)

// lab-05d: clone.provider rds_restore rehearses on a point-in-time restore
// of the monitored RDS instance in clone.rds.region: a full copy, so it
// needs clone.allow_full_copy, and each restore waits at most
// clone.restore_deadline_minutes.

func rdsRestoreConfig() *Config {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "rds_restore"
	cfg.Clone.AllowFullCopy = true
	cfg.Clone.RDS.Region = "us-east-1"
	return cfg
}

func TestCloneRDSDefaults(t *testing.T) {
	c := DefaultConfig().Clone
	if c.RestoreDeadlineMinutes != 120 || DefaultCloneRestoreDeadlineMinutes != 120 ||
		c.RDS.Region != "" || c.RDS.InstanceClass != "" {
		t.Fatalf("defaults: restore_deadline_minutes %d, rds %+v", c.RestoreDeadlineMinutes,
			c.RDS)
	}
	if err := rdsRestoreConfig().validateAgentNative(); err != nil {
		t.Fatalf("rds_restore with a region, the opt-in and defaults: %v", err)
	}
}

// Invalid configuration: a full copy without the opt-in stops startup
// with the setting named, rather than leaving rehearsals silently off.
func TestCloneRDSNeedsAllowFullCopy(t *testing.T) {
	cfg := rdsRestoreConfig()
	cfg.Clone.AllowFullCopy = false
	err := cfg.validateAgentNative()
	if err == nil || !strings.Contains(err.Error(), "clone.allow_full_copy") ||
		!strings.Contains(err.Error(), "rds_restore") {
		t.Fatalf("rds_restore without allow_full_copy = %v", err)
	}
}

// The cost warning is logged before the first restore.
func TestCloneRDSWarnsOfTheFullInstanceCost(t *testing.T) {
	w := rdsRestoreConfig().Clone.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "rds_restore") ||
		!strings.Contains(w[0], "full") || !strings.Contains(w[0], "instance-hours") {
		t.Fatalf("warnings = %q, want one cost warning", w)
	}
}

// Default value masking: a block naming only the region keeps the
// restore deadline default instead of zero.
func TestCloneRDSYAMLKeepsDefaults(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: localhost
  user: postgres
  database: app
clone:
  provider: rds_restore
  allow_full_copy: true
  rds:
    region: eu-west-2
`)
	c := cfg.Clone
	if c.Provider != "rds_restore" || c.RDS.Region != "eu-west-2" ||
		c.RestoreDeadlineMinutes != 120 || c.RDS.InstanceClass != "" || !c.AllowFullCopy {
		t.Fatalf("loaded clone config = %+v", c)
	}
}

func TestCloneRDSYAMLReadsEveryKey(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: localhost
  user: postgres
  database: app
clone:
  provider: rds_restore
  allow_full_copy: true
  restore_deadline_minutes: 240
  rds:
    region: us-gov-west-1
    instance_class: db.t4g.medium
`)
	c := cfg.Clone
	if c.RDS.Region != "us-gov-west-1" || c.RDS.InstanceClass != "db.t4g.medium" ||
		c.RestoreDeadlineMinutes != 240 {
		t.Fatalf("clone = %+v", c)
	}
}

func TestCloneRDSValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*CloneProviderConfig)
		want   string
	}{
		"no region":    {func(c *CloneProviderConfig) { c.RDS.Region = "" }, "region is required"},
		"bad region":   {func(c *CloneProviderConfig) { c.RDS.Region = "useast1" }, "AWS region"},
		"bad class":    {func(c *CloneProviderConfig) { c.RDS.InstanceClass = "large" }, "class"},
		"deadline 29":  {func(c *CloneProviderConfig) { c.RestoreDeadlineMinutes = 29 }, "30-720"},
		"deadline 721": {func(c *CloneProviderConfig) { c.RestoreDeadlineMinutes = 721 }, "30-720"},
		"deadline 0":   {func(c *CloneProviderConfig) { c.RestoreDeadlineMinutes = 0 }, "30-720"},
		"deadline neg": {func(c *CloneProviderConfig) { c.RestoreDeadlineMinutes = -1 }, "30-720"},
	}
	for name, tc := range cases {
		cfg := rdsRestoreConfig()
		tc.mutate(&cfg.Clone)
		err := cfg.validateAgentNative()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	for _, d := range []int{30, 720} {
		cfg := rdsRestoreConfig()
		cfg.Clone.RestoreDeadlineMinutes = d
		if err := cfg.validateAgentNative(); err != nil {
			t.Errorf("deadline %d refused: %v", d, err)
		}
	}
}

// A leftover clone.rds block or deadline never stops another provider.
func TestCloneRDSBlockIgnoredForOtherProviders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clone.RDS.Region = "not a region"
	cfg.Clone.RestoreDeadlineMinutes = 0
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("provider none with a broken rds block: %v", err)
	}
}
