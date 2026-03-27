package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
	// chdir to a temp dir with no config.yaml so auto-detect doesn't fire
	tmp := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	cfg, err := Load([]string{})
	if err != nil {
		t.Fatalf("Load with defaults failed: %v", err)
	}

	if cfg.Mode != "extension" {
		t.Errorf("Mode = %q, want %q", cfg.Mode, "extension")
	}
	if cfg.Postgres.Host != "localhost" {
		t.Errorf("Postgres.Host = %q, want %q", cfg.Postgres.Host, "localhost")
	}
	if cfg.Postgres.Port != 5432 {
		t.Errorf("Postgres.Port = %d, want %d", cfg.Postgres.Port, 5432)
	}
	if cfg.Postgres.MaxConnections != DefaultPGMaxConnections {
		t.Errorf("Postgres.MaxConnections = %d, want %d",
			cfg.Postgres.MaxConnections, DefaultPGMaxConnections)
	}
	if cfg.Collector.IntervalSeconds != 60 {
		t.Errorf("Collector.IntervalSeconds = %d, want 60",
			cfg.Collector.IntervalSeconds)
	}
	if cfg.Collector.BatchSize != 1000 {
		t.Errorf("Collector.BatchSize = %d, want 1000",
			cfg.Collector.BatchSize)
	}
	if cfg.Collector.MaxQueries != DefaultCollectorMaxQueries {
		t.Errorf("Collector.MaxQueries = %d, want %d",
			cfg.Collector.MaxQueries, DefaultCollectorMaxQueries)
	}
	if cfg.Trust.Level != "observation" {
		t.Errorf("Trust.Level = %q, want %q", cfg.Trust.Level, "observation")
	}
	if cfg.LLM.Enabled != false {
		t.Errorf("LLM.Enabled = %v, want false", cfg.LLM.Enabled)
	}
	if cfg.Safety.CPUCeilingPct != 90 {
		t.Errorf("Safety.CPUCeilingPct = %d, want 90",
			cfg.Safety.CPUCeilingPct)
	}
}

func TestConfigPrecedence_CLIOverEnv(t *testing.T) {
	tmp := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	t.Setenv("SAGE_PG_HOST", "env-host")

	cfg, err := Load([]string{"--pg-host=cli-host", "--mode=extension"})
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Postgres.Host != "cli-host" {
		t.Errorf("Postgres.Host = %q, want %q (CLI should override env)",
			cfg.Postgres.Host, "cli-host")
	}
}

func TestConfigPrecedence_DatabaseURL(t *testing.T) {
	tmp := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	dbURL := "postgres://u:p@dburl:5432/db?sslmode=require"
	t.Setenv("SAGE_DATABASE_URL", dbURL)

	cfg, err := Load([]string{"--mode=standalone"})
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Postgres.DSN() != dbURL {
		t.Errorf("DSN() = %q, want %q", cfg.Postgres.DSN(), dbURL)
	}
}

func TestConfigValidation_InvalidTrustLevel(t *testing.T) {
	tmp := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	yamlContent := `trust:
  level: "invalid"
`
	cfgPath := filepath.Join(tmp, "test-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	_, err = Load([]string{"--config=" + cfgPath, "--mode=extension"})
	if err == nil {
		t.Fatal("expected error for invalid trust level, got nil")
	}
	if !strings.Contains(err.Error(), "trust.level") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "trust.level")
	}
}

func TestConfigValidation_ZeroCollectorInterval(t *testing.T) {
	tmp := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	yamlContent := `collector:
  interval_seconds: 0
`
	cfgPath := filepath.Join(tmp, "test-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	_, err = Load([]string{"--config=" + cfgPath, "--mode=extension"})
	if err == nil {
		t.Fatal("expected error for zero collector interval, got nil")
	}
	if !strings.Contains(err.Error(), "collector.interval_seconds") {
		t.Errorf("error = %q, want it to contain %q",
			err.Error(), "collector.interval_seconds")
	}
}

func TestConfigValidation_ZeroMaxQueries(t *testing.T) {
	tmp := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	yamlContent := `collector:
  max_queries: 0
`
	cfgPath := filepath.Join(tmp, "test-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	_, err = Load([]string{"--config=" + cfgPath, "--mode=extension"})
	if err == nil {
		t.Fatal("expected error for zero max_queries, got nil")
	}
	if !strings.Contains(err.Error(), "collector.max_queries") {
		t.Errorf("error = %q, want it to contain %q",
			err.Error(), "collector.max_queries")
	}
}

func TestConfigValidation_InvalidMode(t *testing.T) {
	tmp := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	os.Chdir(tmp)

	_, err = Load([]string{"--mode=bogus"})
	if err == nil {
		t.Fatal("expected error for invalid mode, got nil")
	}
	if !strings.Contains(err.Error(), "invalid mode") {
		t.Errorf("error = %q, want it to contain %q",
			err.Error(), "invalid mode")
	}
}

func TestDSN_BuildsLibpq(t *testing.T) {
	p := PostgresConfig{
		Host:     "myhost",
		Port:     5433,
		User:     "myuser",
		Password: "mypass",
		Database: "mydb",
		SSLMode:  "require",
	}
	want := "host=myhost port=5433 user=myuser password=mypass dbname=mydb sslmode=require"
	if got := p.DSN(); got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}

	// When DatabaseURL is set, it takes precedence
	p.DatabaseURL = "postgres://override@host/db"
	if got := p.DSN(); got != p.DatabaseURL {
		t.Errorf("DSN() = %q, want DatabaseURL %q", got, p.DatabaseURL)
	}
}

func TestApplyHotReload(t *testing.T) {
	target := newDefaults()
	target.Collector.IntervalSeconds = 60

	fresh := newDefaults()
	fresh.Collector.IntervalSeconds = 30

	changed := applyHotReload(target, fresh)

	if target.Collector.IntervalSeconds != 30 {
		t.Errorf("Collector.IntervalSeconds = %d, want 30",
			target.Collector.IntervalSeconds)
	}

	found := false
	for _, c := range changed {
		if c == "collector.interval_seconds" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("changed = %v, want it to contain %q",
			changed, "collector.interval_seconds")
	}
}

func TestApplyHotReload_PostgresNotChanged(t *testing.T) {
	target := newDefaults()
	target.Postgres.Host = "original"

	fresh := newDefaults()
	fresh.Postgres.Host = "new-host"

	applyHotReload(target, fresh)

	if target.Postgres.Host != "original" {
		t.Errorf("Postgres.Host = %q, want %q (postgres should not be hot-reloadable)",
			target.Postgres.Host, "original")
	}
}
