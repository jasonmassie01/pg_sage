package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
)

// everyModeOwners are the reconfiguration owners a sidecar registers in
// every mode: all production owners but the YAML fleet's databases owner.
func everyModeOwners() []string {
	return slices.DeleteFunc(config.ProductionOwners(), func(owner string) bool {
		return owner == config.FleetDatabasesOwner
	})
}

// registryPath maps a config_meta key to the lifecycle registry's path: a
// slice of structs ("x[].y") is one registry field ("x").
func registryPath(key string) string {
	if before, _, found := strings.Cut(key, "[]"); found {
		return before
	}
	return key
}

// lifecycleFor is the lifecycle Settings shows for a config_meta key. A
// top-level key reports what a standalone sidecar applies: a field whose
// owner only the YAML fleet registers fails closed to restart, as
// ConfigController.EffectiveLifecycle does. A per-database key
// ("databases[].x") reports DatabaseFieldLifecycles.
func lifecycleFor(key string) (string, error) {
	if field, ok := strings.CutPrefix(key, "databases[]."); ok {
		entry, found := config.LookupDatabaseFieldLifecycle(registryPath(field))
		if !found {
			return "", fmt.Errorf("config key %q has no per-database lifecycle", key)
		}
		return string(entry.Lifecycle), nil
	}
	entry, found := config.LookupFieldLifecycle(registryPath(key))
	if !found {
		return "", fmt.Errorf("config key %q has no lifecycle in the registry", key)
	}
	if entry.Owner != "" && entry.Lifecycle != config.LifecycleRestart &&
		!slices.Contains(everyModeOwners(), entry.Owner) {
		return string(config.LifecycleRestart), nil
	}
	return string(entry.Lifecycle), nil
}
