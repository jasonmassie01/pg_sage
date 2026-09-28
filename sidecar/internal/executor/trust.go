package executor

import (
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// inMaintenanceWindow reports whether the wall clock is inside the
// configured trust.maintenance_window.
func inMaintenanceWindow(expression string) bool {
	return inMaintenanceWindowAt(expression, time.Now())
}

// inMaintenanceWindowAt evaluates trust.maintenance_window with the single
// window engine, policy.ParseWindow. Unset and the config-only "never"
// aliases are closed. A value that does not parse (possible only for an
// override stored before validation existed) fails closed.
func inMaintenanceWindowAt(expression string, now time.Time) bool {
	if strings.TrimSpace(expression) == "" || config.MaintenanceWindowDisabled(expression) {
		return false
	}
	window, err := policy.ParseWindow(expression)
	return err == nil && window.Contains(now)
}
