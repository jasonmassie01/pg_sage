package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// G5-B02: the removed extension mode left configController nil, so the
// first config.yaml edit dereferenced nil inside the watcher goroutine. A
// mode without a standalone control database must still get a controller.
func TestWatchedConfigApplyWithoutControlPoolAdvancesGeneration(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	oldMeta, oldPool := globalMetaState, pool
	t.Cleanup(func() { globalMetaState, pool = oldMeta, oldPool })
	globalMetaState, pool = nil, nil
	cfg = config.DefaultConfig()
	cfg.Mode = config.ModeMeta
	cfg.Trust.Level = "advisory"
	configController = nil
	fleetMgr = nil

	if err := ensureConfigController(); err != nil {
		t.Fatalf("ensure controller: %v", err)
	}
	if configController == nil {
		t.Fatal("meta mode must still build a config controller")
	}
	before := configController.Desired().Generation
	updated := config.Clone(cfg)
	updated.Trust.Level = "observation"

	if err := applyWatchedConfig(updated); err != nil {
		t.Fatalf("watched config apply: %v", err)
	}

	desired := configController.Desired()
	if desired.Generation != before+1 {
		t.Fatalf("desired generation = %d, want %d", desired.Generation, before+1)
	}
	if desired.Config.Trust.Level != "observation" {
		t.Fatalf("desired trust = %q, want observation", desired.Config.Trust.Level)
	}
	if configControlPool() != nil {
		t.Fatal("meta mode without meta state must not persist into the monitored database")
	}
}

func TestEnsureConfigControllerKeepsExistingController(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	existing := config.NewConfigControllerAtGeneration(cfg, 7, nil)
	configController = existing

	if err := ensureConfigController(); err != nil {
		t.Fatalf("ensure controller: %v", err)
	}
	if configController != existing {
		t.Fatal("ensureConfigController replaced a live controller")
	}
}
