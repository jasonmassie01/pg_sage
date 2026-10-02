package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// defaultUnusedWindow is analyzer.unused_index_window_days when unset.
const defaultUnusedWindow = 7 * 24 * time.Hour

// unusedEvidence is the live state an unused-index drop rests on.
type unusedEvidence struct {
	matches int       // indexes with that schema-qualified name
	scans   int64     // their idx_scan
	epoch   time.Time // the database's relation stats epoch; zero if unknown
}

// unusedEvidenceSQL reads, for one "schema.index", the index's scans and
// the relation stats epoch: the later of pg_stat_database.stats_reset
// (moved by every pg_stat_reset*) and the postmaster start.
const unusedEvidenceSQL = `/* pg_sage */ SELECT
  (SELECT count(*) FROM pg_stat_all_indexes s
    WHERE s.schemaname || '.' || s.indexrelname = $1),
  (SELECT COALESCE(max(s.idx_scan), 0) FROM pg_stat_all_indexes s
    WHERE s.schemaname || '.' || s.indexrelname = $1),
  (SELECT GREATEST(COALESCE(d.stats_reset, '-infinity'::timestamptz),
                   pg_postmaster_start_time())
     FROM pg_stat_database d WHERE d.datname = current_database())`

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readUnusedEvidence reads the live evidence for ident ("schema.index").
func readUnusedEvidence(ctx context.Context, q rowQuerier, ident string) (
	unusedEvidence, error) {
	var ev unusedEvidence
	var epoch *time.Time
	if err := q.QueryRow(ctx, unusedEvidenceSQL, ident).
		Scan(&ev.matches, &ev.scans, &epoch); err != nil {
		return unusedEvidence{}, fmt.Errorf("read unused-index evidence for %s: %w", ident, err)
	}
	if epoch != nil {
		ev.epoch = *epoch
	}
	return ev, nil
}

// broken returns why the evidence does not support a drop at now, or "".
// It holds when the index exists exactly once, has zero scans, and the
// statistics have run, without a reset, for at least the window.
func (u unusedEvidence) broken(now time.Time, window time.Duration) string {
	switch {
	case u.matches == 0:
		return "index not found"
	case u.matches > 1:
		return "index name is ambiguous"
	case u.scans > 0:
		return fmt.Sprintf("index was scanned (%d scans)", u.scans)
	case u.epoch.IsZero():
		return "statistics epoch unknown"
	case now.Sub(u.epoch) < window:
		return fmt.Sprintf("statistics reset at %s, inside the %s unused window",
			u.epoch.UTC().Format(time.RFC3339), window)
	}
	return ""
}

// unusedWindow is the configured unused-index window.
func unusedWindow(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.Analyzer.UnusedIndexWindowDays <= 0 {
		return defaultUnusedWindow
	}
	return time.Duration(cfg.Analyzer.UnusedIndexWindowDays) * 24 * time.Hour
}

// unusedDropRefused re-checks an unused-index finding's evidence live and
// reports whether the drop must not proceed: the analyzer resolves a
// finding whose window contains a reset on its next cycle, but this cycle
// may come first. A failed read refuses (fail closed).
func (e *Executor) unusedDropRefused(ctx context.Context, f analyzer.Finding) bool {
	if f.Category != "unused_index" {
		return false
	}
	var reason string
	if e.pool == nil {
		reason = "no database to read it from"
	} else if ev, err := readUnusedEvidence(ctx, e.pool, f.ObjectIdentifier); err != nil {
		reason = err.Error()
	} else {
		reason = ev.broken(time.Now(), unusedWindow(e.cfg))
	}
	if reason == "" {
		return false
	}
	e.logFn("executor", "not dropping %s: unused-index evidence does not hold: %s",
		f.ObjectIdentifier, reason)
	return true
}
