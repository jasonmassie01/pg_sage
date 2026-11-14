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
//
// candidate must be the file layer (Load, without overrides). An override
// that replaces an explicit file value is returned as masked and recorded
// on candidate; for the trust ceilings the more restrictive value applies
// (config-03).
func ReplayStoredOverrides(
	candidate *config.Config, overrides []store.ConfigOverride,
	warn func(msg string),
) []config.MaskedKey {
	base := config.Clone(candidate)
	var masked []config.MaskedKey
	for _, override := range overrides {
		value, ok, problem := store.ReplayOverride(override.Key, override.Value)
		if problem != "" && warn != nil {
			warn(store.StoredOverrideWarning(override, problem))
		}
		if !ok {
			continue
		}
		ApplyConfigOverrideSnapshot(candidate, override.Key, value)
		if m, isMasked := maskedOverride(candidate, base, override.Key,
			override.UpdatedBy, override.UpdatedAt); isMasked {
			masked = append(masked, m)
		}
	}
	return recordMasked(candidate, base, masked)
}

// logWatchEnabled parses a validated logwatch.enabled: auto is unset;
// anything else unparsable fails closed to off, as atob does (rca-09b).
func logWatchEnabled(s string) *bool {
	enabled, err := config.ParseLogWatchEnabled(s)
	if err != nil {
		slog.Warn("hotReload: logwatch.enabled is not true, false or auto; applying false")
		off := false
		return &off
	}
	return enabled
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
