package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-02, error propagation on real PostgreSQL: rows that predate
// validation (or were written by hand) no longer poison the startup config
// and every later PUT.

// storeRawOverride writes a global row the override store would refuse.
func storeRawOverride(t *testing.T, pool *pgxpool.Pool, key, value string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.config (key, value, database_id)
		VALUES ($1, $2, NULL)
		ON CONFLICT (key, COALESCE(database_id, 0)) DO UPDATE SET value = $2`,
		key, value); err != nil {
		t.Fatalf("store raw override %s: %v", key, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config
			WHERE key = $1 AND database_id IS NULL`, key)
	})
}

func TestStartupSkipsInvalidStoredOverridesAndPutStillWorks(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	pool := preflightRuntimePool(t)
	storeRawOverride(t, pool, "collector.interval_seconds", "abc")
	storeRawOverride(t, pool, "tuner.llm_enabled", "false")
	storeRawOverride(t, pool, "emergency_stop", "false") // runtime state, not an override
	cfg = config.DefaultConfig()
	configController = nil
	var initErr error
	out := captureStderr(t, func() { initErr = initializeConfigController(pool) })
	if initErr != nil {
		t.Fatalf("initializeConfigController: %v", initErr)
	}
	if cfg.Collector.IntervalSeconds != 60 {
		t.Errorf("collector interval = %d, want the default 60 (the bad row skipped)",
			cfg.Collector.IntervalSeconds)
	}
	if !cfg.Tuner.LLMEnabled {
		t.Error("stored tuner.llm_enabled=false was applied; it is not a settable key")
	}
	for _, key := range []string{"collector.interval_seconds", "tuner.llm_enabled"} {
		if !strings.Contains(out, key) {
			t.Errorf("no startup warning names %s: %q", key, out)
		}
	}
	if strings.Contains(out, "emergency_stop") {
		t.Errorf("runtime state row emergency_stop was reported as an override: %q", out)
	}
	if strings.Contains(out, "abc") {
		t.Errorf("startup warning repeats a stored value: %q", out)
	}
	putAnotherKey(t, pool)
}

// putAnotherKey is the controlled PUT path for analyzer.interval_seconds.
func putAnotherKey(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	cs := store.NewConfigStore(pool)
	desired := configController.Desired()
	candidate := desired.Config
	api.ApplyConfigOverrideSnapshot(candidate, "analyzer.interval_seconds", "120")
	writes := []store.ConfigOverrideWrite{{Key: "analyzer.interval_seconds", Value: "120"}}
	t.Cleanup(func() {
		_ = cs.DeleteOverride(context.Background(), "analyzer.interval_seconds", 0)
	})
	_, err := configController.ApplyWithPersistence(context.Background(),
		desired.Generation, candidate,
		func(ctx context.Context, _ config.ConfigSnapshot) error {
			_, err := cs.SetOverridesCAS(ctx, writes, 0, 0, desired.Generation)
			return err
		})
	if err != nil {
		t.Fatalf("PUT analyzer.interval_seconds after a skipped row: %v", err)
	}
	// analyzer.interval_seconds is restart-bound: it lands in the desired
	// revision and waits there for the restart.
	after := configController.Desired()
	if after.Generation != desired.Generation+1 ||
		after.Config.Analyzer.IntervalSeconds != 120 {
		t.Fatalf("desired after PUT = generation %d interval %d, want %d and 120",
			after.Generation, after.Config.Analyzer.IntervalSeconds, desired.Generation+1)
	}
}

// config-02 (b): rows that pass one by one but leave an invalid whole fail
// startup naming the key, instead of a sidecar whose every PUT fails.
func TestStartupFailsNamingTheKeyWhenStoredOverridesBreakTheConfig(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	pool := preflightRuntimePool(t)
	storeRawOverride(t, pool, "clone.provider", "dle")
	cfg = config.DefaultConfig()
	cfg.Clone.DLEEndpoint = ""
	configController = nil
	var initErr error
	captureStderr(t, func() { initErr = initializeConfigController(pool) })
	if initErr == nil {
		t.Fatal("startup accepted stored clone.provider=dle without an endpoint")
	}
	if !strings.Contains(initErr.Error(), "clone.dle_endpoint") {
		t.Fatalf("startup error %q does not name clone.dle_endpoint", initErr)
	}
	if configController != nil {
		t.Fatal("a controller was built from an invalid config")
	}
}

// Reload (watcher) path: the candidate loader returns the error, so the
// watcher keeps the previous configuration.
func TestReloadCandidateRejectsStoredOverridesThatBreakTheConfig(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	pool := preflightRuntimePool(t)
	storeRawOverride(t, pool, "clone.provider", "dle")
	candidate := config.DefaultConfig()
	candidate.Clone.DLEEndpoint = ""
	err := applyPersistedGlobalOverrides(candidate, pool)
	if err == nil || !strings.Contains(err.Error(), "clone.dle_endpoint") {
		t.Fatalf("replay error = %v, want it to name clone.dle_endpoint", err)
	}
}
