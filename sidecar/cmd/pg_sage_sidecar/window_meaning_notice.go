package main

import "github.com/pg-sage/sidecar/internal/config"

// noticeWindowMeaningChange tells the operator, once at startup, when the
// configured maintenance window means something different under the
// unified window grammar (D2).
func noticeWindowMeaningChange(value string, logf func(component, msg string, args ...any)) {
	note := config.MaintenanceWindowMeaningChange(value)
	if note == "" {
		return
	}
	logf("config", "trust.maintenance_window %q: %s. Check that it is still "+
		"the window you intend.", value, note)
}
