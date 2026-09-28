package main

import "context"

type windowWarningSource interface {
	MaintenanceWindowOverrideWarnings(context.Context) ([]string, error)
}

// warnInvalidStoredWindows logs each stored trust.maintenance_window
// override that no longer validates (saved before validation existed). It
// changes nothing: the runtime keeps treating such a window as closed.
func warnInvalidStoredWindows(
	ctx context.Context, source windowWarningSource,
	logf func(component, msg string, args ...any),
) {
	warnings, err := source.MaintenanceWindowOverrideWarnings(ctx)
	if err != nil {
		logf("config", "could not check stored maintenance-window overrides: %v", err)
		return
	}
	for _, warning := range warnings {
		logf("config", "%s", warning)
	}
}
