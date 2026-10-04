package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgRejectionStore keeps rejections in sage.optimizer_rejection, scoped to
// the connected database (current_database()).
type pgRejectionStore struct{ pool *pgxpool.Pool }

func newPGRejectionStore(pool *pgxpool.Pool) *pgRejectionStore {
	return &pgRejectionStore{pool: pool}
}

// The unique key's leading columns serve the lookup.
const recentRejectionsSQL = `/* pg_sage */
SELECT method, key_cols, predicate, include_cols, ddl, improvement_pct,
       min_improvement_pct, reason, workload, row_estimate, measure_count, measured_at
FROM sage.optimizer_rejection
WHERE database_name = current_database() AND schema_name = $1 AND table_name = $2
  AND measured_at >= now() - make_interval(secs => $3)
ORDER BY measured_at DESC, id DESC
LIMIT $4`

func (s *pgRejectionStore) recent(ctx context.Context, schema, table string,
	maxAge time.Duration, limit int) ([]rejection, error) {
	rows, err := s.pool.Query(ctx, recentRejectionsSQL, schema, table, maxAge.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("load what-if rejections for %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []rejection
	for rows.Next() {
		r := rejection{Schema: schema, Table: table}
		var workload []byte
		if err := rows.Scan(&r.Shape.Method, &r.Shape.Keys, &r.Shape.Predicate,
			&r.Shape.Include, &r.DDL, &r.ImprovementPct, &r.MinImprovementPct, &r.Reason,
			&workload, &r.RowEstimate, &r.MeasureCount, &r.MeasuredAt); err != nil {
			return nil, fmt.Errorf("load what-if rejections for %s.%s: %w", schema, table, err)
		}
		if err := json.Unmarshal(workload, &r.Workload); err != nil {
			return nil, fmt.Errorf("decode what-if rejection workload for %s.%s: %w",
				schema, table, err)
		}
		if len(r.Shape.Include) == 0 {
			r.Shape.Include = nil
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load what-if rejections for %s.%s: %w", schema, table, err)
	}
	return out, nil
}

// A repeated measurement of the same shape replaces the evidence and
// counts the measurement; concurrent cycles serialize on the unique key.
const recordRejectionSQL = `/* pg_sage */
INSERT INTO sage.optimizer_rejection (schema_name, table_name, shape_hash, method,
    key_cols, predicate, include_cols, ddl, improvement_pct, min_improvement_pct,
    reason, workload, row_estimate)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13)
ON CONFLICT (database_name, schema_name, table_name, shape_hash) DO UPDATE SET
    ddl = EXCLUDED.ddl,
    improvement_pct = EXCLUDED.improvement_pct,
    min_improvement_pct = EXCLUDED.min_improvement_pct,
    reason = EXCLUDED.reason,
    workload = EXCLUDED.workload,
    row_estimate = EXCLUDED.row_estimate,
    measure_count = sage.optimizer_rejection.measure_count + 1,
    measured_at = now()`

func (s *pgRejectionStore) record(ctx context.Context, r rejection) error {
	workload := r.Workload
	if workload == nil {
		workload = []workloadSample{}
	}
	raw, err := json.Marshal(workload)
	if err != nil {
		return fmt.Errorf("encode what-if rejection workload: %w", err)
	}
	include := r.Shape.Include
	if include == nil {
		include = []string{}
	}
	_, err = s.pool.Exec(ctx, recordRejectionSQL, r.Schema, r.Table, r.Shape.hash(),
		r.Shape.Method, r.Shape.Keys, r.Shape.Predicate, include, r.DDL, r.ImprovementPct,
		r.MinImprovementPct, r.Reason, string(raw), r.RowEstimate)
	if err != nil {
		return fmt.Errorf("record what-if rejection on %s.%s: %w", r.Schema, r.Table, err)
	}
	return nil
}
