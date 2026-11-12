package config

import (
	"os"
	"path/filepath"
	"testing"
)

// apiauth-08: api.database_grants turns on per-database authorization in
// fleet mode. Off by default, so an install keeps today's behaviour.

func TestDatabaseGrants_DefaultOffWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.DatabaseGrants {
		t.Fatal("api.database_grants defaults to true, want false")
	}
	if DefaultConfig().API.DatabaseGrants {
		t.Fatal("DefaultConfig enables api.database_grants")
	}
}

func TestDatabaseGrants_LoadsFromYAML(t *testing.T) {
	chdirTemp(t)
	for raw, want := range map[string]bool{"true": true, "false": false} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		yaml := "api:\n  database_grants: " + raw + "\n"
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load([]string{"--config", path})
		if err != nil {
			t.Fatalf("Load %s: %v", raw, err)
		}
		if cfg.API.DatabaseGrants != want {
			t.Fatalf("database_grants: %s loaded as %v", raw, cfg.API.DatabaseGrants)
		}
	}
}

func TestDatabaseGrants_IsRestartBound(t *testing.T) {
	got, ok := LookupFieldLifecycle("api.database_grants")
	if !ok || got.Lifecycle != LifecycleRestart {
		t.Fatalf("api.database_grants lifecycle = %+v (found %v), want restart", got, ok)
	}
}
