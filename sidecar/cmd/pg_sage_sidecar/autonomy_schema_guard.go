package main

import (
	"time"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
)

// schemaGuardOptions takes the idle window for leftover clone families
// from analyzer.unused_index_window_days, the analyzer's own idle window,
// so the schema guard and the analyzer agree on what a leftover is.
func schemaGuardOptions(cfg *config.Config) autonomy.SchemaGuardOptions {
	days := cfg.Analyzer.UnusedIndexWindowDays
	if days <= 0 {
		days = config.DefaultUnusedIndexWindowDays
	}
	return autonomy.SchemaGuardOptions{IdleWindow: time.Duration(days) * 24 * time.Hour}
}

func autonomyDDLDebounce(cfg *config.Config) time.Duration {
	return cfg.Analyzer.SchemaGuardDDLDebounce()
}
