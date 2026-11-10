package config

import (
	"os"
	"path/filepath"
	"testing"
)

// apiauth-12: api.access_log is a *bool so an absent key means enabled.

func loadAccessLogYAML(t *testing.T, yaml string) *Config {
	t.Helper()
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestAccessLog_DefaultIsOnWithoutAConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.AccessLog != nil || !cfg.API.AccessLogEnabled() {
		t.Fatalf("default access_log = %v, enabled %v; want unset and on",
			cfg.API.AccessLog, cfg.API.AccessLogEnabled())
	}
	if !(APIConfig{}).AccessLogEnabled() {
		t.Fatal("zero APIConfig has the access log off")
	}
}

func TestAccessLog_YAMLFalseAndTrue(t *testing.T) {
	cfg := loadAccessLogYAML(t, "api:\n  listen_addr: \":8080\"\n")
	if !cfg.API.AccessLogEnabled() {
		t.Fatal("api section without access_log turned the log off")
	}
	cfg = loadAccessLogYAML(t, "api:\n  access_log: false\n")
	if cfg.API.AccessLog == nil || cfg.API.AccessLogEnabled() {
		t.Fatalf("access_log: false gave %v", cfg.API.AccessLog)
	}
	cfg = loadAccessLogYAML(t, "api:\n  access_log: true\n")
	if !cfg.API.AccessLogEnabled() {
		t.Fatal("access_log: true is off")
	}
}

func TestAccessLog_CloneIsDeep(t *testing.T) {
	off := false
	src := &Config{API: APIConfig{AccessLog: &off}}
	cp := Clone(src)
	if cp.API.AccessLog == src.API.AccessLog {
		t.Fatal("Clone shares the access_log pointer")
	}
	*src.API.AccessLog = true
	if cp.API.AccessLogEnabled() {
		t.Fatal("changing the source changed the clone")
	}
	if Clone(&Config{}).API.AccessLog != nil {
		t.Fatal("Clone of an unset access_log is not nil")
	}
}
