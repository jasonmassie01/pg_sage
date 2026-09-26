package api

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
)

// configBaseSource yields the file-derived config (YAML + env + flags,
// without persisted overrides) that override deletes rebuild from.
type configBaseSource func() (*config.Config, error)

// staticConfigBase serves a fixed snapshot; used by embedders and tests
// that have no config file to reload.
func staticConfigBase(base *config.Config) configBaseSource {
	snapshot := config.Clone(base)
	return func() (*config.Config, error) {
		if snapshot == nil {
			return nil, fmt.Errorf("config base is unavailable")
		}
		return config.Clone(snapshot), nil
	}
}

// runtimeConfigBase prefers the live loader so a delete rebases on the
// CURRENT file config; the startup clone would silently revert values the
// YAML watcher has since applied, such as a trust downgrade (G5-B03).
func runtimeConfigBase(
	loader func() (*config.Config, error), startup, cfg *config.Config,
) configBaseSource {
	if loader != nil {
		return func() (*config.Config, error) {
			base, err := loader()
			if err != nil {
				return nil, fmt.Errorf("load current config file: %w", err)
			}
			if base == nil {
				return nil, fmt.Errorf("load current config file: no config")
			}
			return base, nil
		}
	}
	if startup != nil {
		return staticConfigBase(startup)
	}
	return staticConfigBase(cfg)
}
