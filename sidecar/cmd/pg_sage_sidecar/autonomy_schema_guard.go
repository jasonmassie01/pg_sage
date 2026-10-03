package main

import (
	"context"
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

// schemaGuardIndexTimeout bounds the concurrent build of the schema guard
// history index; a build that cannot finish (a long-open transaction) is
// cancelled and retried at the next start.
const schemaGuardIndexTimeout = 30 * time.Minute

// ensureSchemaGuardIndexLogged ensures the schema guard history index in
// the background. A failure is logged and never stops the sidecar: the
// guard's one history query per cycle works without it, only slower.
func ensureSchemaGuardIndexLogged(
	ctx context.Context, database string, ensure func(context.Context) error,
	warn func(string, ...any),
) {
	ctx, cancel := context.WithTimeout(ctx, schemaGuardIndexTimeout)
	defer cancel()
	if err := ensure(ctx); err != nil {
		warn("database %s: ensure schema guard history index "+
			"(sage.idx_decision_schema_guard_targets) failed, retried at next start: %v",
			database, err)
	}
}
