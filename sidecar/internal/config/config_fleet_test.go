package config

import (
	"os"
	"path/filepath"
	"testing"
)

// helper to write temp YAML and load it
func loadFromYAML(t *testing.T, yaml string) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"-config", path})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfig_StandaloneNormalization(t *testing.T) {
	cfg := loadFromYAML(t, `
mode: standalone
postgres:
  host: localhost
  port: 5432
  user: postgres
  password: secret
  database: mydb
  sslmode: prefer
  max_connections: 3
`)
	if len(cfg.Databases) != 1 {
		t.Fatalf("expected 1 database, got %d", len(cfg.Databases))
	}
	if cfg.Databases[0].Name != "mydb" {
		t.Errorf("expected name mydb, got %s", cfg.Databases[0].Name)
	}
	if cfg.Mode != "standalone" {
		t.Errorf("expected standalone, got %s", cfg.Mode)
	}
}

func TestConfig_StandaloneNormalization_PreservesAllFields(t *testing.T) {
	cfg := loadFromYAML(t, `
mode: standalone
postgres:
  host: db.example.com
  port: 5433
  user: admin
  password: hunter2
  database: proddb
  sslmode: require
  max_connections: 5
`)
	d := cfg.Databases[0]
	if d.Host != "db.example.com" {
		t.Errorf("host: got %s", d.Host)
	}
	if d.Port != 5433 {
		t.Errorf("port: got %d", d.Port)
	}
	if d.User != "admin" {
		t.Errorf("user: got %s", d.User)
	}
	if d.Password != "hunter2" {
		t.Errorf("password: got %s", d.Password)
	}
	if d.Database != "proddb" {
		t.Errorf("database: got %s", d.Database)
	}
	if d.SSLMode != "require" {
		t.Errorf("sslmode: got %s", d.SSLMode)
	}
	if d.MaxConnections != 5 {
		t.Errorf("max_connections: got %d", d.MaxConnections)
	}
}

func TestConfig_FleetMode_ParsesDatabases(t *testing.T) {
	cfg := loadFromYAML(t, `
mode: fleet
databases:
  - name: prod-orders
    host: orders.db.internal
    port: 5432
    user: sage
    password: pass1
    database: orders
    sslmode: require
  - name: prod-users
    host: users.db.internal
    port: 5432
    user: sage
    password: pass2
    database: users
    sslmode: require
  - name: staging
    host: staging.db.internal
    port: 5432
    user: sage
    password: pass3
    database: staging
    sslmode: prefer
`)
	if len(cfg.Databases) != 3 {
		t.Fatalf("expected 3 databases, got %d", len(cfg.Databases))
	}
	if cfg.Mode != "fleet" {
		t.Errorf("mode: got %s", cfg.Mode)
	}
	if cfg.Databases[0].Name != "prod-orders" {
		t.Errorf("db0 name: got %s", cfg.Databases[0].Name)
	}
	if cfg.Databases[1].Host != "users.db.internal" {
		t.Errorf("db1 host: got %s", cfg.Databases[1].Host)
	}
	if cfg.Databases[2].SSLMode != "prefer" {
		t.Errorf("db2 sslmode: got %s", cfg.Databases[2].SSLMode)
	}
}

func TestConfig_FleetMode_AppliesDefaults(t *testing.T) {
	cfg := loadFromYAML(t, `
mode: fleet
defaults:
  max_connections: 4
  collector_interval_seconds: 120
  analyzer_interval_seconds: 900
databases:
  - name: db1
    host: h1
    port: 5432
    user: u1
    password: p1
    database: d1
`)
	d := cfg.Databases[0]
	if d.MaxConnections != 4 {
		t.Errorf("expected max_connections=4, got %d", d.MaxConnections)
	}
	if d.CollectorIntervalSeconds != 120 {
		t.Errorf("expected collector_interval=120, got %d",
			d.CollectorIntervalSeconds)
	}
	if d.AnalyzerIntervalSeconds != 900 {
		t.Errorf("expected analyzer_interval=900, got %d",
			d.AnalyzerIntervalSeconds)
	}
}

func TestConfig_FleetMode_OverrideBeatsDefault(t *testing.T) {
	cfg := loadFromYAML(t, `
mode: fleet
defaults:
  collector_interval_seconds: 120
databases:
  - name: db1
    host: h1
    port: 5432
    user: u1
    password: p1
    database: d1
    collector_interval_seconds: 30
`)
	if cfg.Databases[0].CollectorIntervalSeconds != 30 {
		t.Errorf("expected 30, got %d",
			cfg.Databases[0].CollectorIntervalSeconds)
	}
}

func TestConfig_FleetMode_Tags(t *testing.T) {
	cfg := loadFromYAML(t, `
mode: fleet
databases:
  - name: db1
    host: h1
    port: 5432
    user: u1
    password: p1
    database: d1
    tags: [production, critical, us-east-1]
`)
	d := cfg.Databases[0]
	if !d.HasTag("critical") {
		t.Error("expected HasTag(critical) = true")
	}
	if d.HasTag("staging") {
		t.Error("expected HasTag(staging) = false")
	}
}

func TestConfig_FleetMode_ConnString(t *testing.T) {
	d := DatabaseConfig{
		User:     "sage",
		Password: "secret",
		Host:     "db.example.com",
		Port:     5432,
		Database: "mydb",
		SSLMode:  "require",
	}
	want := "postgres://sage:secret@db.example.com:5432/mydb?sslmode=require"
	if got := d.ConnString(); got != want {
		t.Errorf("ConnString():\n  got  %s\n  want %s", got, want)
	}
}

func TestConfig_FleetMode_PerDatabaseTrust(t *testing.T) {
	cfg := loadFromYAML(t, `
mode: fleet
databases:
  - name: db1
    host: h1
    port: 5432
    user: u
    password: p
    database: d1
    trust_level: observation
  - name: db2
    host: h2
    port: 5432
    user: u
    password: p
    database: d2
    trust_level: autonomous
  - name: db3
    host: h3
    port: 5432
    user: u
    password: p
    database: d3
    trust_level: advisory
`)
	if cfg.Databases[0].TrustLevel != "observation" {
		t.Errorf("db1 trust: %s", cfg.Databases[0].TrustLevel)
	}
	if cfg.Databases[1].TrustLevel != "autonomous" {
		t.Errorf("db2 trust: %s", cfg.Databases[1].TrustLevel)
	}
	if cfg.Databases[2].TrustLevel != "advisory" {
		t.Errorf("db3 trust: %s", cfg.Databases[2].TrustLevel)
	}
}

func TestConfig_FleetMode_ExecutorDisabled(t *testing.T) {
	f := false
	d1 := DatabaseConfig{ExecutorEnabled: &f}
	if d1.IsExecutorEnabled() {
		t.Error("expected disabled")
	}
	d2 := DatabaseConfig{}
	if !d2.IsExecutorEnabled() {
		t.Error("expected default enabled")
	}
}

func TestConfig_FleetMode_LLMDisabledPerDB(t *testing.T) {
	f := false
	d1 := DatabaseConfig{LLMEnabled: &f}
	if d1.IsLLMEnabled() {
		t.Error("expected disabled")
	}
	d2 := DatabaseConfig{}
	if !d2.IsLLMEnabled() {
		t.Error("expected default enabled")
	}
}

func TestConfig_FleetMode_Validation_DuplicateNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
mode: fleet
databases:
  - name: prod
    host: h1
    port: 5432
    user: u
    password: p
    database: d1
  - name: prod
    host: h2
    port: 5432
    user: u
    password: p
    database: d2
`
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load([]string{"-config", path})
	if err == nil {
		t.Fatal("expected error for duplicate names")
	}
}

func TestConfig_FleetMode_Validation_EmptyName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
mode: fleet
databases:
  - name: ""
    host: h1
    port: 5432
    user: u
    password: p
    database: d1
`
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load([]string{"-config", path})
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestConfig_FleetMode_Validation_EmptyHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
mode: fleet
databases:
  - name: db1
    host: ""
    port: 5432
    user: u
    password: p
    database: d1
`
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load([]string{"-config", path})
	if err == nil {
		t.Fatal("expected error for empty host")
	}
}

func TestConfig_FleetMode_Validation_NoDatabases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
mode: fleet
databases: []
`
	os.WriteFile(path, []byte(yaml), 0644)
	_, err := Load([]string{"-config", path})
	if err == nil {
		t.Fatal("expected error for empty databases")
	}
}

func TestConfig_IsFleet(t *testing.T) {
	cfg := &Config{Mode: "fleet"}
	if !cfg.IsFleet() {
		t.Error("expected IsFleet() true")
	}
	cfg.Mode = "standalone"
	if cfg.IsFleet() {
		t.Error("expected IsFleet() false")
	}
}
