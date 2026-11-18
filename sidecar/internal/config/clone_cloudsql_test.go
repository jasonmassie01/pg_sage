package config

import (
	"strings"
	"testing"
)

// lab-05e: clone.provider cloudsql_clone rehearses on a Cloud SQL clone of
// clone.cloudsql.instance in clone.cloudsql.project: a full copy, so it
// needs clone.allow_full_copy, and each clone waits at most
// clone.restore_deadline_minutes.

func cloudSQLCloneConfig() *Config {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "cloudsql_clone"
	cfg.Clone.AllowFullCopy = true
	cfg.Clone.CloudSQL.Project = "acme-prod-1234"
	cfg.Clone.CloudSQL.Instance = "shop"
	return cfg
}

func TestCloneCloudSQLDefaults(t *testing.T) {
	c := DefaultConfig().Clone
	if c.CloudSQL.Project != "" || c.CloudSQL.Instance != "" ||
		c.RestoreDeadlineMinutes != DefaultCloneRestoreDeadlineMinutes {
		t.Fatalf("defaults: cloudsql %+v, deadline %d", c.CloudSQL, c.RestoreDeadlineMinutes)
	}
	if err := cloudSQLCloneConfig().validateAgentNative(); err != nil {
		t.Fatalf("cloudsql_clone with a project, instance, the opt-in and defaults: %v", err)
	}
}

// Invalid configuration: a full copy without the opt-in stops startup.
func TestCloneCloudSQLNeedsAllowFullCopy(t *testing.T) {
	cfg := cloudSQLCloneConfig()
	cfg.Clone.AllowFullCopy = false
	err := cfg.validateAgentNative()
	if err == nil || !strings.Contains(err.Error(), "clone.allow_full_copy") ||
		!strings.Contains(err.Error(), "cloudsql_clone") {
		t.Fatalf("cloudsql_clone without allow_full_copy = %v", err)
	}
}

func TestCloneCloudSQLWarnsOfTheFullInstanceCost(t *testing.T) {
	w := cloudSQLCloneConfig().Clone.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "cloudsql_clone") ||
		!strings.Contains(w[0], "full") || !strings.Contains(w[0], "instance-hours") {
		t.Fatalf("warnings = %q, want one cost warning", w)
	}
}

// Default value masking: a block naming only the instance keeps the
// restore deadline default instead of zero.
func TestCloneCloudSQLYAMLKeepsDefaults(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: 10.0.0.5
  user: postgres
  database: app
clone:
  provider: cloudsql_clone
  allow_full_copy: true
  cloudsql:
    project: acme-prod-1234
    instance: shop
`)
	c := cfg.Clone
	if c.Provider != "cloudsql_clone" || c.CloudSQL.Project != "acme-prod-1234" ||
		c.CloudSQL.Instance != "shop" || c.RestoreDeadlineMinutes != 120 || !c.AllowFullCopy {
		t.Fatalf("loaded clone config = %+v", c)
	}
}

func TestCloneCloudSQLValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*CloneProviderConfig)
		want   string
	}{
		"no project": {func(c *CloneProviderConfig) { c.CloudSQL.Project = "" },
			"clone.cloudsql.project is required"},
		"bad project": {func(c *CloneProviderConfig) { c.CloudSQL.Project = "Acme_Prod" },
			"project id"},
		"no instance": {func(c *CloneProviderConfig) { c.CloudSQL.Instance = "" },
			"clone.cloudsql.instance is required"},
		"bad instance": {func(c *CloneProviderConfig) { c.CloudSQL.Instance = "Shop" },
			"instance name"},
		"clone name": {func(c *CloneProviderConfig) {
			c.CloudSQL.Instance = "sage-rehearsal-0a1b2c3d"
		}, "pg_sage clone"},
		"deadline 29":  {func(c *CloneProviderConfig) { c.RestoreDeadlineMinutes = 29 }, "30-720"},
		"deadline 721": {func(c *CloneProviderConfig) { c.RestoreDeadlineMinutes = 721 }, "30-720"},
		"deadline 0":   {func(c *CloneProviderConfig) { c.RestoreDeadlineMinutes = 0 }, "30-720"},
	}
	for name, tc := range cases {
		cfg := cloudSQLCloneConfig()
		tc.mutate(&cfg.Clone)
		err := cfg.validateAgentNative()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	for _, d := range []int{30, 720} {
		cfg := cloudSQLCloneConfig()
		cfg.Clone.RestoreDeadlineMinutes = d
		if err := cfg.validateAgentNative(); err != nil {
			t.Errorf("deadline %d refused: %v", d, err)
		}
	}
}

// A leftover clone.cloudsql block never stops another provider.
func TestCloneCloudSQLBlockIgnoredForOtherProviders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clone.CloudSQL.Project = "not a project"
	cfg.Clone.CloudSQL.Instance = "Not/An/Instance"
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("provider none with a broken cloudsql block: %v", err)
	}
}
