package analyzer

import (
	"context"
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/histstore"
)

// categorySageFootprint is the finding raised when pg_sage's own tables
// take more than retention.sage_size_warning_pct of the database it
// guards (dogfood lifeos: sage.snapshots alone reached 9.3 GB).
const categorySageFootprint = "sage_footprint"

// sageFootprintTop is how many of the largest sage tables a finding names.
const sageFootprintTop = 5

// sageTableSize is one sage table's total size (heap, TOAST, indexes).
type sageTableSize struct {
	name  string
	bytes int64
}

// sageFootprint is the measured size of the sage schema in one database.
type sageFootprint struct {
	database string
	total    int64
	tables   []sageTableSize // largest first
}

const sageFootprintSQL = `/* pg_sage */
SELECT current_database(), n.nspname || '.' || c.relname, pg_total_relation_size(c.oid)
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'sage' AND c.relkind IN ('r', 'm')
 ORDER BY 3 DESC, 2`

// checkSageFootprint measures the sage schema against the database size
// collected this cycle. A disabled guard (0) evaluates without a query so
// an open finding resolves; an unknown database size or a failed query
// fails the category so an open finding stays open.
func (a *Analyzer) checkSageFootprint(
	ctx context.Context, current *collector.Snapshot,
) []Finding {
	limit := a.cfg.Retention.SageSizeWarningPct
	if limit <= 0 {
		a.eval.evaluated(categorySageFootprint)
		return nil
	}
	if current == nil || current.System.DBSizeBytes <= 0 {
		a.evalFail(categorySageFootprint)
		a.logFn("WARN", "analyzer: sage footprint skipped: database size unknown "+
			"this cycle")
		return nil
	}
	fp, err := a.measureSageFootprint(ctx)
	if err != nil {
		a.evalFail(categorySageFootprint)
		a.logFn("WARN", "analyzer: measure sage schema size: %v", err)
		return nil
	}
	a.eval.evaluated(categorySageFootprint)
	return annotateHistoryPlacement(ruleSageFootprint(fp, current.System.DBSizeBytes, limit),
		histstore.Resolve(a.pool).Mode())
}

func (a *Analyzer) measureSageFootprint(ctx context.Context) (sageFootprint, error) {
	var fp sageFootprint
	rows, err := a.catalog().Query(ctx, sageFootprintSQL)
	if err != nil {
		return fp, err
	}
	defer rows.Close()
	for rows.Next() {
		var t sageTableSize
		if err := rows.Scan(&fp.database, &t.name, &t.bytes); err != nil {
			return fp, fmt.Errorf("scan sage table size: %w", err)
		}
		fp.total += t.bytes
		fp.tables = append(fp.tables, t)
	}
	return fp, rows.Err()
}

// ruleSageFootprint raises a warning when the sage schema is more than
// limitPct percent of the database. A non-positive limit disables it; an
// unknown database size never raises it, and neither does a sage schema
// below the snapshot cap's floor: on a small database a fresh install's own
// tables are a large share of it, which is no problem worth a finding.
func ruleSageFootprint(fp sageFootprint, dbBytes int64, limitPct int) []Finding {
	if limitPct <= 0 || dbBytes <= 0 || fp.total < config.MinSnapshotCapBytes {
		return nil
	}
	// Integer comparison: exactly at the limit is fine.
	if fp.total*100 <= dbBytes*int64(limitPct) {
		return nil
	}
	share := math.Round(float64(fp.total)*1000/float64(dbBytes)) / 10
	largest := make([]map[string]any, 0, sageFootprintTop)
	for i, t := range fp.tables {
		if i == sageFootprintTop {
			break
		}
		largest = append(largest, map[string]any{"table": t.name, "bytes": t.bytes})
	}
	return []Finding{{
		Category:         categorySageFootprint,
		Severity:         "warning",
		ObjectType:       "database",
		ObjectIdentifier: fp.database,
		Title: fmt.Sprintf("Sage data uses %.1f%% of database %s (limit %d%%)",
			share, fp.database, limitPct),
		Detail: map[string]any{
			"sage_bytes":     fp.total,
			"database_bytes": dbBytes,
			"share_pct":      share,
			"limit_pct":      limitPct,
			"largest_tables": largest,
		},
		Recommendation: "Shorten retention.snapshots_days (or the retention of the " +
			"largest table listed), or raise retention.sage_size_warning_pct if this " +
			"share is expected. Catalog snapshots are stored as deltas; full rows written " +
			"by earlier versions age out with retention.snapshots_days.",
	}}
}
