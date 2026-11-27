package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-05: replica B (this process's globals) converges on the global
// revision replica A saved to the shared control database.

type resyncTestLLMOwner struct{}

func (resyncTestLLMOwner) Name() string { return config.LLMOwner }

func (resyncTestLLMOwner) Prepare(
	context.Context, config.ConfigSnapshot, config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	return resyncTestPrepared{}, nil
}

type resyncTestPrepared struct{}

func (resyncTestPrepared) Commit(context.Context) error   { return nil }
func (resyncTestPrepared) Rollback(context.Context) error { return nil }
func (resyncTestPrepared) Drain(context.Context) error    { return nil }

// startResyncReplica builds replica B through the startup path, on a YAML
// file and the shared control database.
func startResyncReplica(t *testing.T) *pgxpool.Pool {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	shared := preflightRuntimePool(t)
	previousPool, previousMeta, previousArgs := pool, globalMetaState, os.Args
	previousCtx := shutdownCtx
	t.Cleanup(func() {
		pool, globalMetaState, os.Args = previousPool, previousMeta, previousArgs
		shutdownCtx = previousCtx
	})
	resetResyncRows(t, shared)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "mode: standalone\npostgres:\n  host: db.example\n  database: app\n  user: u\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"pg_sage", "--config", path}
	loaded, err := loadFileConfigBase()
	if err != nil {
		t.Fatalf("load replica config: %v", err)
	}
	cfg, pool, globalMetaState, configController = loaded, shared, nil, nil
	shutdownCtx = context.Background()
	if err := initializeConfigController(shared); err != nil {
		t.Fatalf("initializeConfigController: %v", err)
	}
	if err := configController.RegisterOwner(resyncTestLLMOwner{}); err != nil {
		t.Fatal(err)
	}
	return shared
}

func resetResyncRows(t *testing.T, shared *pgxpool.Pool) {
	t.Helper()
	reset := func() {
		_, _ = shared.Exec(context.Background(), `DELETE FROM sage.config
			WHERE database_id IS NULL AND key IN ('llm.timeout_seconds',
			'clone.provider', '__pg_sage_config_generation')`)
	}
	reset()
	t.Cleanup(reset)
}

// replicaASaves is another sidecar's PUT: an override revision under CAS.
func replicaASaves(t *testing.T, shared *pgxpool.Pool, expected uint64, timeout string) {
	t.Helper()
	writes := []store.ConfigOverrideWrite{{Key: "llm.timeout_seconds", Value: timeout}}
	if _, err := store.NewConfigStore(shared).SetOverridesCAS(
		context.Background(), writes, 0, 0, expected,
	); err != nil {
		t.Fatalf("replica A save: %v", err)
	}
}

func setRawGlobalRow(t *testing.T, shared *pgxpool.Pool, key, value string) {
	t.Helper()
	if _, err := shared.Exec(context.Background(), `INSERT INTO sage.config
		(key, value, database_id) VALUES ($1, $2, NULL)
		ON CONFLICT (key, COALESCE(database_id, 0)) DO UPDATE SET value = $2`,
		key, value); err != nil {
		t.Fatalf("set raw row %s: %v", key, err)
	}
}

func TestResyncTickAdoptsAnotherReplicasSave(t *testing.T) {
	shared := startResyncReplica(t)
	replicaASaves(t, shared, 1, "77")
	if got := configController.Active().Config.LLM.TimeoutSeconds; got == 77 {
		t.Fatal("replica B saw the new value before any resync")
	}
	before := time.Now()
	var adopted bool
	out := captureStderr(t, func() { adopted = resyncConfigFromStore(context.Background()) })
	if !adopted {
		t.Fatalf("resync tick did not adopt generation 2; log: %q", out)
	}
	active := configController.Active()
	if active.Generation != 2 || active.Config.LLM.TimeoutSeconds != 77 {
		t.Fatalf("replica B = generation %d timeout %d, want 2 and 77",
			active.Generation, active.Config.LLM.TimeoutSeconds)
	}
	first := configController.LastResync()
	if first.Before(before) {
		t.Fatalf("LastResync = %v, want after %v", first, before)
	}
	if !strings.Contains(out, "generation") {
		t.Fatalf("resync log does not name the generation: %q", out)
	}
	if resyncConfigFromStore(context.Background()) {
		t.Fatal("a second tick at the same generation adopted again")
	}
	if !configController.LastResync().Equal(first) {
		t.Fatal("LastResync moved on a no-op tick")
	}
}

func TestResyncTickWarnsAndKeepsConfigWhenGenerationUnreadable(t *testing.T) {
	shared := startResyncReplica(t)
	setRawGlobalRow(t, shared, "__pg_sage_config_generation", "not-a-number")
	var adopted bool
	out := captureStderr(t, func() { adopted = resyncConfigFromStore(context.Background()) })
	if adopted {
		t.Fatal("resync adopted although the generation is unreadable")
	}
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "durable config generation") {
		t.Fatalf("no WARN naming the durable generation: %q", out)
	}
	if got := configController.Desired().Generation; got != 1 {
		t.Fatalf("desired generation = %d, want the unchanged 1", got)
	}
}

func TestResyncTickRejectsAnInvalidDurableRevisionNamingTheKey(t *testing.T) {
	shared := startResyncReplica(t)
	setRawGlobalRow(t, shared, "clone.provider", "dle")
	setRawGlobalRow(t, shared, "__pg_sage_config_generation", "5")
	var adopted bool
	out := captureStderr(t, func() { adopted = resyncConfigFromStore(context.Background()) })
	if adopted {
		t.Fatal("resync adopted a revision that fails validation")
	}
	if !strings.Contains(out, "ERROR") || !strings.Contains(out, "clone.dle_endpoint") {
		t.Fatalf("no ERROR naming clone.dle_endpoint: %q", out)
	}
	if got := configController.Desired(); got.Generation != 1 ||
		got.Config.Clone.Provider == "dle" {
		t.Fatalf("controller changed: generation %d provider %q",
			got.Generation, got.Config.Clone.Provider)
	}
	if !configController.LastResync().IsZero() {
		t.Fatal("LastResync moved for a rejected revision")
	}
}

// The YAML reload path on a stale replica: the file changes, replica A
// saves before the reload applies, the store refuses the CAS, and the
// replica resyncs (re-reading this file and the overrides). The reload is
// acknowledged and A's override is not reverted.
func TestWatchedReloadOnStaleReplicaResyncsInsteadOfFailing(t *testing.T) {
	shared := startResyncReplica(t)
	yaml := "mode: standalone\npostgres:\n  host: db.example\n  database: app\n" +
		"  user: u\nanalyzer:\n  interval_seconds: 777\n"
	if err := os.WriteFile(os.Args[2], []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := loadConfigCandidate()
	if err != nil {
		t.Fatalf("load reload candidate: %v", err)
	}
	replicaASaves(t, shared, 1, "77")
	captureStderr(t, func() { err = applyWatchedConfig(updated) })
	if err != nil {
		t.Fatalf("watched reload on a stale replica: %v", err)
	}
	desired := configController.Desired()
	if desired.Generation != 2 || desired.Config.Analyzer.IntervalSeconds != 777 {
		t.Fatalf("replica B desired = generation %d analyzer %d, want 2 and 777",
			desired.Generation, desired.Config.Analyzer.IntervalSeconds)
	}
	if got := configController.Active().Config.LLM.TimeoutSeconds; got != 77 {
		t.Fatalf("replica B timeout = %d, want replica A's 77 (not reverted)", got)
	}
}

func TestResyncLoopConvergesAndStopsWithContext(t *testing.T) {
	shared := startResyncReplica(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { configResyncLoop(ctx, 20*time.Millisecond); close(done) }()
	replicaASaves(t, shared, 1, "79")
	deadline := time.Now().Add(10 * time.Second)
	for configController.Active().Config.LLM.TimeoutSeconds != 79 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the resync loop did not adopt replica A's save within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the resync loop did not stop when its context ended")
	}
}

// Nil/empty: a YAML fleet has no control database, so no poll starts and a
// tick without a controller does nothing.
func TestResyncWithoutControlPoolStartsNoPoll(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	previousPool, previousMeta := pool, globalMetaState
	t.Cleanup(func() { pool, globalMetaState = previousPool, previousMeta })
	cfg = config.DefaultConfig()
	cfg.Mode = "fleet"
	pool, globalMetaState, configController = nil, nil, nil
	if resyncConfigFromStore(context.Background()) {
		t.Fatal("a tick without a controller reported a resync")
	}
	configController = config.NewConfigController(cfg, nil)
	if startConfigResync(context.Background()) {
		t.Fatal("a resync poll started without a control database")
	}
	if resyncConfigFromStore(context.Background()) {
		t.Fatal("a tick without a durable source reported a resync")
	}
}
