package config

import (
	"strings"
	"testing"
)

// market-12: databases[].cnpg (and postgres.cnpg in standalone mode) names
// the CloudNativePG Cluster behind a database; it makes the provider cnpg.

// No concurrent access tests: validation is a pure function of the config.

func TestCNPGConfigValid(t *testing.T) {
	c := CNPGConfig{Cluster: "pg-main", Namespace: "db"}
	for _, provider := range []string{"", "self-managed"} {
		if err := validateCNPG("databases[0]", provider, c,
			CloudResourceConfig{}); err != nil {
			t.Errorf("provider %q: %v", provider, err)
		}
	}
	if !c.Enabled() || (CNPGConfig{}).Enabled() {
		t.Fatal("Enabled reports a configured block only")
	}
	if err := validateCNPG("x", "rds", CNPGConfig{}, CloudResourceConfig{}); err != nil {
		t.Fatalf("an empty block is not validated against the provider: %v", err)
	}
}

func TestCNPGConfigInvalid(t *testing.T) {
	gcp := CloudResourceConfig{GCP: CloudResourceGCP{Instance: "main"}}
	for _, tc := range []struct {
		name     string
		provider string
		c        CNPGConfig
		r        CloudResourceConfig
		want     string
	}{
		{"cluster only", "", CNPGConfig{Cluster: "pg-main"}, CloudResourceConfig{},
			"set cluster and namespace together"},
		{"namespace only", "", CNPGConfig{Namespace: "db"}, CloudResourceConfig{},
			"set cluster and namespace together"},
		{"upper case", "", CNPGConfig{Cluster: "PG", Namespace: "db"},
			CloudResourceConfig{}, `cnpg.cluster "PG"`},
		{"path", "", CNPGConfig{Cluster: "pg-main", Namespace: "db/../kube-system"},
			CloudResourceConfig{}, `cnpg.namespace "db/../kube-system"`},
		{"too long", "", CNPGConfig{Cluster: strings.Repeat("a", 64), Namespace: "db"},
			CloudResourceConfig{}, "cnpg.cluster"},
		{"managed provider", "rds", CNPGConfig{Cluster: "pg-main", Namespace: "db"},
			CloudResourceConfig{}, `provider "rds"`},
		{"with a cloud resource", "", CNPGConfig{Cluster: "pg-main", Namespace: "db"}, gcp,
			"cloud_resource"},
	} {
		err := validateCNPG("databases[\"app\"]", tc.provider, tc.c, tc.r)
		if err == nil || !strings.Contains(err.Error(), tc.want) ||
			!strings.Contains(err.Error(), `databases["app"]`) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestCNPGConfigLoadsFromYAML(t *testing.T) {
	cfg, err := loadYAMLResult(t, "mode: standalone\npostgres:\n  host: pg-main-rw\n"+
		"  cnpg:\n    cluster: pg-main\n    namespace: db\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := CNPGConfig{Cluster: "pg-main", Namespace: "db"}
	if cfg.Postgres.CNPG != want || len(cfg.Databases) != 1 ||
		cfg.Databases[0].CNPG != want {
		t.Fatalf("postgres.cnpg %+v databases %+v", cfg.Postgres.CNPG, cfg.Databases)
	}
	_, err = loadYAMLResult(t, "mode: standalone\npostgres:\n  host: db\n"+
		"  cnpg:\n    cluster: pg-main\n")
	if err == nil || !strings.Contains(err.Error(), "postgres.cnpg") {
		t.Fatalf("Load error = %v, want postgres.cnpg named", err)
	}
}
