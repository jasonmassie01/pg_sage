package store

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
)

const maintenanceWindowKey = "trust.maintenance_window"

// InvalidMaintenanceWindowOverrides returns one warning per stored
// trust.maintenance_window override that no longer validates. Such rows
// predate validation on save. The runtime treats them as a closed window
// (fail closed); the warning names the value and how to fix it.
func InvalidMaintenanceWindowOverrides(overrides []ConfigOverride) []string {
	var warnings []string
	for _, override := range overrides {
		if override.Key != maintenanceWindowKey {
			continue
		}
		err := config.ValidateMaintenanceWindow(override.Value)
		if err == nil {
			continue
		}
		scope, path := "global", "/api/v1/config/global/"+maintenanceWindowKey
		if override.DatabaseID > 0 {
			scope = fmt.Sprintf("database %d", override.DatabaseID)
			path = fmt.Sprintf("/api/v1/config/databases/%d/%s",
				override.DatabaseID, maintenanceWindowKey)
		}
		warnings = append(warnings, fmt.Sprintf(
			"stored %s override %s=%q is invalid, so the maintenance window stays "+
				"closed: %v. Fix it by saving a valid window in Settings or delete "+
				"the override with DELETE %s", scope, maintenanceWindowKey,
			override.Value, err, path))
	}
	return warnings
}

// MaintenanceWindowOverrideWarnings checks the stored overrides of every
// scope (global and per database).
func (s *ConfigStore) MaintenanceWindowOverrideWarnings(
	ctx context.Context,
) ([]string, error) {
	overrides, err := s.GetOverrides(ctx, -1)
	if err != nil {
		return nil, fmt.Errorf("read stored config overrides: %w", err)
	}
	return InvalidMaintenanceWindowOverrides(overrides), nil
}
