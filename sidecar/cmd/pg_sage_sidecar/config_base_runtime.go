package main

import (
	"os"

	"github.com/pg-sage/sidecar/internal/config"
)

// loadFileConfigBase reloads the CURRENT file config (YAML + env + flags)
// without persisted overrides, carrying the runtime facts detected at
// startup. Override deletes rebase on it, so a value the YAML watcher has
// since applied (e.g. a trust downgrade) is not reverted to the startup
// clone (G5-B03).
func loadFileConfigBase() (*config.Config, error) {
	base, err := config.Load(os.Args[1:])
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		base.CloudEnvironment = cfg.CloudEnvironment
		base.PGVersionNum = cfg.PGVersionNum
		base.HasWALColumns = cfg.HasWALColumns
		base.HasPlanTimeColumns = cfg.HasPlanTimeColumns
	}
	return base, nil
}
