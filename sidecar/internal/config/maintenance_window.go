package config

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/policy"
)

// MaintenanceWindowDisabled reports the config-only aliases that close the
// maintenance window: never, off, none and disabled. Policy documents have
// no such value; a policy always names at least one window.
func MaintenanceWindowDisabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "never", "off", "none", "disabled":
		return true
	}
	return false
}

// ValidateMaintenanceWindow checks trust.maintenance_window against the
// policy window grammar. Empty (unset) and the "never" aliases are valid.
func ValidateMaintenanceWindow(value string) error {
	if strings.TrimSpace(value) == "" || MaintenanceWindowDisabled(value) {
		return nil
	}
	if _, err := policy.ParseWindow(value); err != nil {
		return fmt.Errorf("trust.maintenance_window: %w (or never to disable)", err)
	}
	return nil
}
