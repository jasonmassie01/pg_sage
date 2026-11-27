package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// closedMaintenanceWindow is what an invalid stored window replays as: the
// executor already treats such a row as a closed window (fail closed).
const closedMaintenanceWindow = "never"

// ReplayOverride decides what one persisted override contributes when it is
// replayed onto a configuration (config-02). A row whose key the store no
// longer accepts, or whose value no longer validates, is skipped (ok false).
// An invalid trust.maintenance_window is the exception: it replays as
// "never", keeping the window closed. problem explains a skipped or replaced
// row without repeating its value, which may be a credential.
func ReplayOverride(key, value string) (apply string, ok bool, problem string) {
	if !strings.Contains(key, ".") {
		// Runtime state shares sage.config (emergency_stop, trust_ramp_start,
		// the fleet stop rows); it is not an override and needs no warning.
		return "", false, ""
	}
	spec, known := config.LookupKey(key)
	if !known || !spec.Overridable {
		return "", false, "the key is not a settable override"
	}
	if validateSpec(spec, value) == nil {
		return value, true, ""
	}
	if key == maintenanceWindowKey {
		return closedMaintenanceWindow, true,
			"the window does not parse, so it stays closed (never)"
	}
	return "", false, fmt.Sprintf("the value fails the %s check", spec.Type)
}

// StoredOverrideWarning is the operator message for a row ReplayOverride
// skipped or replaced: the key, its scope, when it was written and how to
// remove it, never the value.
func StoredOverrideWarning(o ConfigOverride, problem string) string {
	scope, path := "global", "/api/v1/config/global/"+o.Key
	if o.DatabaseID > 0 {
		scope = fmt.Sprintf("database %d", o.DatabaseID)
		path = fmt.Sprintf("/api/v1/config/databases/%d/%s", o.DatabaseID, o.Key)
	}
	updated := "unknown"
	if !o.UpdatedAt.IsZero() {
		updated = o.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("stored %s config override %s (updated_at %s) is not applied "+
		"as stored: %s. Save a valid value in Settings or remove it with DELETE %s",
		scope, o.Key, updated, problem, path)
}
