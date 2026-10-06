package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// history.store: where pg_sage keeps its telemetry history (snapshots and
// the query store). monitored (default) keeps it in each monitored
// database; meta keeps it in the meta database, which needs meta_db.

func TestHistoryStore_DefaultsToMonitoredWithoutConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.History.Store != HistoryStoreMonitored {
		t.Fatalf("history.store = %q, want %q", cfg.History.Store, HistoryStoreMonitored)
	}
	if cfg.HistoryInMeta() {
		t.Fatal("the default must keep history in the monitored database")
	}
	if DefaultConfig().History.Store != HistoryStoreMonitored {
		t.Fatal("DefaultConfig disagrees with Load(nil)")
	}
}

func TestHistoryStore_EmptyValueMeansMonitored(t *testing.T) {
	cfg, err := loadRCAYAML(t, "history:\n  store: \"\"\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HistoryInMeta() {
		t.Fatal("an empty history.store must mean monitored")
	}
}

func TestHistoryStore_MetaNeedsAMetaDatabase(t *testing.T) {
	_, err := loadRCAYAML(t, "history:\n  store: meta\n")
	if err == nil || !strings.Contains(err.Error(), "history.store") ||
		!strings.Contains(err.Error(), "meta_db") {
		t.Fatalf("history.store meta without meta_db: want an error naming both keys, got %v",
			err)
	}
}

func TestHistoryStore_MetaWithMetaDatabase(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "mode: fleet\nmeta_db: postgres://sage@meta.example/sage_meta\n" +
		"history:\n  store: meta\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.HistoryInMeta() || cfg.History.Store != HistoryStoreMeta {
		t.Fatalf("history.store = %q, want meta", cfg.History.Store)
	}
}

func TestHistoryStore_RejectsUnknownValues(t *testing.T) {
	for _, v := range []string{"Meta", "remote", "s3", "monitored "} {
		_, err := loadRCAYAML(t, "history:\n  store: \""+v+"\"\n")
		if err == nil || !strings.Contains(err.Error(), "history.store") {
			t.Fatalf("history.store %q: want a validation error, got %v", v, err)
		}
	}
}

func TestHistoryStore_IsRestartBoundAndClassified(t *testing.T) {
	lc, ok := LookupFieldLifecycle("history.store")
	if !ok {
		t.Fatal("history.store has no lifecycle entry")
	}
	if lc.Lifecycle != LifecycleRestart {
		t.Fatalf("history.store lifecycle = %s, want restart (moving history needs a "+
			"migration, never a hot reload)", lc.Lifecycle)
	}
	if got, ok := KeyClassOf("history.store"); !ok || got != KeyOperatorPreference {
		t.Fatalf("history.store class = %q (%v), want operator_preference", got, ok)
	}
}
