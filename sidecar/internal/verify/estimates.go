package verify

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// estimatePlansPerTarget bounds the plans read per target and window.
const estimatePlansPerTarget = 20

// targetPlansSQL reads the newest plans of each target captured in the
// window: one range of idx_explain_queryid (queryid, captured_at DESC) per
// target.
const targetPlansSQL = `/* pg_sage */ SELECT p.plan_json::text
	FROM unnest($1::bigint[]) AS q(id)
	CROSS JOIN LATERAL (
		SELECT e.plan_json FROM sage.explain_cache e
		WHERE e.queryid = q.id AND e.captured_at >= $2 AND e.captured_at <= $3
		ORDER BY e.captured_at DESC LIMIT $4) p`

// EstimateErrors summarizes the row-estimate error of the targets' plans
// with actual rows captured between from and to (sage.explain_cache;
// plans without actual rows, EXPLAIN without ANALYZE, are not evidence).
func (s *PostgresObservationSource) EstimateErrors(
	ctx context.Context, ids []int64, from, to time.Time,
) (EstimateSample, error) {
	if s == nil || s.queryer == nil {
		return EstimateSample{}, errors.New("verify observation pool is unavailable")
	}
	if len(ids) == 0 {
		return EstimateSample{}, nil
	}
	rows, err := s.queryer.Query(ctx, targetPlansSQL, ids, from, to, estimatePlansPerTarget)
	if err != nil {
		return EstimateSample{}, fmt.Errorf("read sampled plans: %w", err)
	}
	defer rows.Close()
	var qerrors []float64
	for rows.Next() {
		var plan string
		if err := rows.Scan(&plan); err != nil {
			return EstimateSample{}, fmt.Errorf("scan sampled plan: %w", err)
		}
		if q, ok := PlanQError([]byte(plan)); ok {
			qerrors = append(qerrors, q)
		}
	}
	if err := rows.Err(); err != nil {
		return EstimateSample{}, fmt.Errorf("read sampled plans: %w", err)
	}
	return SummarizeEstimates(qerrors), nil
}

// IndexFootprint is the size, in bytes, of a REINDEX target and whether it
// is valid: one index, or (table) every index of a table, valid only when
// all are. A target that does not exist is an error, never a zero size.
func (s *PostgresObservationSource) IndexFootprint(
	ctx context.Context, target string, table bool,
) (int64, bool, error) {
	if s == nil || s.queryer == nil {
		return 0, false, errors.New("verify observation pool is unavailable")
	}
	query := `/* pg_sage */ SELECT count(*), COALESCE(sum(pg_relation_size(i.indexrelid)), 0),
		COALESCE(bool_and(i.indisvalid), false)
		FROM pg_index i WHERE i.indexrelid = to_regclass($1)`
	if table {
		query = `/* pg_sage */ SELECT CASE WHEN to_regclass($1) IS NULL THEN 0 ELSE 1 END,
			COALESCE(sum(pg_relation_size(i.indexrelid)), 0),
			COALESCE(bool_and(i.indisvalid), true)
			FROM pg_index i WHERE i.indrelid = to_regclass($1)`
	}
	var found int
	var bytes int64
	var valid bool
	if err := s.queryer.QueryRow(ctx, query, target).Scan(&found, &bytes, &valid); err != nil {
		return 0, false, fmt.Errorf("read index footprint of %s: %w", target, err)
	}
	if found == 0 {
		return 0, false, fmt.Errorf("REINDEX target %s not found", target)
	}
	return bytes, valid, nil
}
