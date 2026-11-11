package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/slo"
	"github.com/pg-sage/sidecar/internal/value"
)

// The metrics refresher's sources. Each runs under metricsSourceTimeout
// and carries the /* pg_sage */ tag, so its load shows in
// pg_stat_statements at the refresh cadence, not the scrape rate.

// valueMetricsSource reads the value ledger counts /metrics exports.
func valueMetricsSource(name string, p *pgxpool.Pool) metricsSource {
	return metricsSource{name: metricsSourceValue,
		read: func(ctx context.Context, s *metricsSnapshot) error {
			read, err := value.ReadMetrics(ctx, value.Source{Name: name, Pool: p})
			if err != nil {
				return err
			}
			s.value = read
			return nil
		}}
}

// sloStatusReader is the SLO engine's read (an *slo.Engine).
type sloStatusReader interface {
	Statuses(ctx context.Context) ([]slo.Status, error)
}

// sloMetricsSource reads the database's SLO error-budget state.
func sloMetricsSource(r sloStatusReader) metricsSource {
	return metricsSource{name: metricsSourceSLO,
		read: func(ctx context.Context, s *metricsSnapshot) error {
			sts, err := r.Statuses(ctx)
			if err != nil {
				return fmt.Errorf("read SLO state: %w", err)
			}
			s.slo = sts
			return nil
		}}
}

const optimizerCountsSQL = `/* pg_sage */ SELECT category, count(*)
	FROM sage.findings
	WHERE status = 'open'
	  AND category IN ('missing_index','covering_index','partial_index','composite_index')
	GROUP BY category`

// optimizerMetricsSource counts open index recommendations by category.
func optimizerMetricsSource(p *pgxpool.Pool) metricsSource {
	return metricsSource{name: metricsSourceOptimizer,
		read: func(ctx context.Context, s *metricsSnapshot) error {
			counts, err := queryCounts(ctx, p, optimizerCountsSQL)
			if err != nil {
				return fmt.Errorf("count open index recommendations: %w", err)
			}
			s.optimizer = counts
			return nil
		}}
}

var errNoMetricsPool = errors.New("database has no connection pool")

// queryCounts reads (label, count) rows into a map.
func queryCounts(ctx context.Context, p *pgxpool.Pool, sql string) (map[string]int64, error) {
	if p == nil {
		return nil, errNoMetricsPool
	}
	rows, err := p.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var label string
		var n int64
		if err := rows.Scan(&label, &n); err != nil {
			return nil, err
		}
		out[label] = n
	}
	return out, rows.Err()
}
