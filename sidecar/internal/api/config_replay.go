package api

import (
	"log/slog"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// ReplayStoredOverrides applies persisted overrides onto candidate, skipping
// each row the override store would refuse today (an unknown key, or a value
// that fails its type) and reporting it through warn, which never sees the
// value. Without this a hand-written "abc" became 0 and failed every later
// PUT (config-02). warn may be nil.
func ReplayStoredOverrides(
	candidate *config.Config, overrides []store.ConfigOverride,
	warn func(msg string),
) {
	for _, override := range overrides {
		value, ok, problem := store.ReplayOverride(override.Key, override.Value)
		if problem != "" && warn != nil {
			warn(store.StoredOverrideWarning(override, problem))
		}
		if ok {
			ApplyConfigOverrideSnapshot(candidate, override.Key, value)
		}
	}
}

// atob parses a validated boolean with the API's grammar; anything else is
// false, so a setter reached without validation fails closed to off.
func atob(s string) bool {
	b, err := config.ParseBool(s)
	if err != nil {
		slog.Warn("hotReload: value is not a boolean; applying false")
		return false
	}
	return b
}
