package value

import (
	"context"
	"fmt"
)

// MetricsRead is the part of one database's ledger that /metrics exports:
// net verified toil by feature and avoided incidents by kind. Identity
// names the physical database so a database registered twice is exported
// once (see dedupeReads).
type MetricsRead struct {
	Identity             string
	ToilMinutesByFeature map[string]float64
	IncidentsByKind      map[string]int64
}

// incidentCountsSQL counts the ledger by kind in SQL; /metrics never needs
// the rows /api/v1/value lists (observability-03).
const incidentCountsSQL = `/* pg_sage */ SELECT kind, count(*)
	FROM sage.incident_avoided GROUP BY kind`

// ReadMetrics reads source's ledger for the metrics refresher with the
// same honesty rules as the full read: only verified successes count and
// reverted actions contribute nothing. Errors name the database.
func ReadMetrics(ctx context.Context, source Source) (MetricsRead, error) {
	if source.Name == "" {
		return MetricsRead{}, ErrUnnamedSource
	}
	if source.Pool == nil {
		return MetricsRead{}, fmt.Errorf("%w: %s", ErrSourceUnavailable, source.Name)
	}
	identity, err := physicalIdentity(ctx, source.Pool)
	if err != nil {
		return MetricsRead{}, fmt.Errorf("read value metrics identity for %s: %w",
			source.Name, err)
	}
	realized := Snapshot{ByFeatureMinutes: map[string]float64{}}
	if err := readRealized(ctx, source.Pool, source.Name, Filter{}, &realized); err != nil {
		return MetricsRead{}, fmt.Errorf("read value metrics toil for %s: %w",
			source.Name, err)
	}
	incidents, err := IncidentCounts(ctx, source.Pool)
	if err != nil {
		return MetricsRead{}, fmt.Errorf("read value metrics incidents for %s: %w",
			source.Name, err)
	}
	return MetricsRead{Identity: identity, ToilMinutesByFeature: realized.ByFeatureMinutes,
		IncidentsByKind: incidents}, nil
}

// IncidentCounts returns the number of avoided incidents per kind; an
// empty ledger is an empty map.
func IncidentCounts(ctx context.Context, q querier) (map[string]int64, error) {
	rows, err := q.Query(ctx, incidentCountsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var kind string
		var n int64
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		counts[kind] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}
