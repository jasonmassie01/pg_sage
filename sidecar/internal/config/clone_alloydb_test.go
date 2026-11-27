package config

import (
	"strings"
	"testing"
)

// lab-05f: clone.provider alloydb_restore rehearses on a point-in-time
// restore of clone.alloydb.cluster in clone.alloydb.project and
// clone.alloydb.location: a full copy, so it needs clone.allow_full_copy,
// and each restore waits at most clone.restore_deadline_minutes.

func alloyDBRestoreConfig() *Config {
	cfg := DefaultConfig()
	cfg.Clone.Provider = "alloydb_restore"
	cfg.Clone.AllowFullCopy = true
	cfg.Clone.AlloyDB = CloneAlloyDBConfig{Project: "acme-prod-1234",
		Location: "us-central1", Cluster: "shop"}
	return cfg
}

func TestCloneAlloyDBDefaults(t *testing.T) {
	c := DefaultConfig().Clone
	if c.AlloyDB != (CloneAlloyDBConfig{}) ||
		c.RestoreDeadlineMinutes != DefaultCloneRestoreDeadlineMinutes {
		t.Fatalf("defaults: alloydb %+v, deadline %d", c.AlloyDB, c.RestoreDeadlineMinutes)
	}
	if err := alloyDBRestoreConfig().validateAgentNative(); err != nil {
		t.Fatalf("alloydb_restore with a project, location, cluster and the opt-in: %v", err)
	}
}

// Invalid configuration: a full copy without the opt-in stops startup.
func TestCloneAlloyDBNeedsAllowFullCopy(t *testing.T) {
	cfg := alloyDBRestoreConfig()
	cfg.Clone.AllowFullCopy = false
	err := cfg.validateAgentNative()
	if err == nil || !strings.Contains(err.Error(), "clone.allow_full_copy") ||
		!strings.Contains(err.Error(), "alloydb_restore") {
		t.Fatalf("alloydb_restore without allow_full_copy = %v", err)
	}
}

func TestCloneAlloyDBWarnsOfTheFullClusterCost(t *testing.T) {
	w := alloyDBRestoreConfig().Clone.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "alloydb_restore") ||
		!strings.Contains(w[0], "full") || !strings.Contains(w[0], "instance-hours") {
		t.Fatalf("warnings = %q, want one cost warning", w)
	}
}

// Default value masking: a block naming only the cluster keeps the
// restore deadline default instead of zero.
func TestCloneAlloyDBYAMLKeepsDefaults(t *testing.T) {
	cfg := loadFromYAML(t, `
postgres:
  host: 10.0.0.5
  user: postgres
  database: app
clone:
  provider: alloydb_restore
  allow_full_copy: true
  alloydb:
    project: acme-prod-1234
    location: us-central1
    cluster: shop
`)
	c := cfg.Clone
	if c.Provider != "alloydb_restore" || c.AlloyDB != (CloneAlloyDBConfig{
		Project: "acme-prod-1234", Location: "us-central1", Cluster: "shop"}) ||
		c.RestoreDeadlineMinutes != 120 || !c.AllowFullCopy {
		t.Fatalf("loaded clone config = %+v", c)
	}
}

func TestCloneAlloyDBValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*CloneAlloyDBConfig)
		want   string
	}{
		"no project":   {func(a *CloneAlloyDBConfig) { a.Project = "" }, "project is required"},
		"bad project":  {func(a *CloneAlloyDBConfig) { a.Project = "Acme_Prod" }, "project id"},
		"no location":  {func(a *CloneAlloyDBConfig) { a.Location = "" }, "location is required"},
		"zone":         {func(a *CloneAlloyDBConfig) { a.Location = "us-central1-a" }, "region"},
		"bad location": {func(a *CloneAlloyDBConfig) { a.Location = "US/central" }, "region"},
		"no cluster":   {func(a *CloneAlloyDBConfig) { a.Cluster = "" }, "cluster is required"},
		"bad cluster":  {func(a *CloneAlloyDBConfig) { a.Cluster = "Shop" }, "cluster id"},
		"long cluster": {func(a *CloneAlloyDBConfig) { a.Cluster = strings.Repeat("a", 64) },
			"cluster id"},
		"clone name": {func(a *CloneAlloyDBConfig) { a.Cluster = "sage-rehearsal-0a1b2c3d" },
			"pg_sage clone"},
	}
	for name, tc := range cases {
		cfg := alloyDBRestoreConfig()
		tc.mutate(&cfg.Clone.AlloyDB)
		err := cfg.validateAgentNative()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	for d, ok := range map[int]bool{0: false, 29: false, 30: true, 720: true, 721: false} {
		cfg := alloyDBRestoreConfig()
		cfg.Clone.RestoreDeadlineMinutes = d
		if err := cfg.validateAgentNative(); (err == nil) != ok {
			t.Errorf("deadline %d: err = %v, want accepted %v", d, err, ok)
		}
	}
	cfg := alloyDBRestoreConfig()
	cfg.Clone.AlloyDB.Cluster = strings.Repeat("a", 63)
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("63-character cluster id refused: %v", err)
	}
}

// A leftover clone.alloydb block never stops another provider.
func TestCloneAlloyDBBlockIgnoredForOtherProviders(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Clone.AlloyDB = CloneAlloyDBConfig{Project: "not a project", Location: "x",
		Cluster: "Not/A/Cluster"}
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("provider none with a broken alloydb block: %v", err)
	}
}
