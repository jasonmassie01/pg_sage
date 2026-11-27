package querystore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// planFlipsSQL reads, per query, the newest fingerprint at or before $2
// and the first different one after it, up to $3: two ranges of
// (queryid, captured_at), however long the window.
const planFlipsSQL = `/* pg_sage */ SELECT q.id, b.captured_at, b.plan_hash,
	d.captured_at, d.plan_hash
	FROM unnest($1::int8[]) AS q(id)
	LEFT JOIN LATERAL (
		SELECT s.captured_at, s.plan_hash FROM sage.query_store s
		WHERE {db:s} AND s.queryid = q.id AND s.plan_hash IS NOT NULL
		  AND s.captured_at <= $2
		ORDER BY s.captured_at DESC LIMIT 1) b ON true
	LEFT JOIN LATERAL (
		SELECT s.captured_at, s.plan_hash FROM sage.query_store s
		WHERE {db:s} AND s.queryid = q.id AND s.plan_hash IS NOT NULL
		  AND s.captured_at > $2 AND s.captured_at <= $3
		  AND s.plan_hash IS DISTINCT FROM b.plan_hash
		ORDER BY s.captured_at LIMIT 1) d ON true`

// PlanFlip is one query's plan fingerprint at or before a time and the
// first different one after it (verification v2's mechanism gate,
// lab-01b). An empty hash (with a zero time) means none was sampled.
type PlanFlip struct {
	QueryID           int64
	BeforeAt, AfterAt time.Time
	Before, After     string
}

// PlanFlips reads the flips of ids around at, up to to, in db's history
// store (histstore.Resolve), with one statement. A query without any
// fingerprint is absent. No ids reads nothing.
func PlanFlips(ctx context.Context, db any, ids []int64, at, to time.Time) (
	map[int64]PlanFlip, error) {
	if to.Before(at) {
		return nil, fmt.Errorf("plan fingerprints of %d queries: window ends (%v) before "+
			"it starts (%v)", len(ids), to, at)
	}
	out := make(map[int64]PlanFlip, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := histstore.Resolve(db).Query(ctx, planFlipsSQL, ids, at, to)
	if err != nil {
		return nil, fmt.Errorf("plan fingerprints of %d queries: %w", len(ids), err)
	}
	var id int64
	var beforeAt, afterAt *time.Time
	var before, after *string
	_, err = pgx.ForEachRow(rows, []any{&id, &beforeAt, &before, &afterAt, &after},
		func() error {
			f := PlanFlip{QueryID: id}
			if beforeAt != nil && before != nil {
				f.BeforeAt, f.Before = beforeAt.UTC(), *before
			}
			if afterAt != nil && after != nil {
				f.AfterAt, f.After = afterAt.UTC(), *after
			}
			if f.Before != "" || f.After != "" {
				out[id] = f
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("plan fingerprints of %d queries: %w", len(ids), err)
	}
	return out, nil
}
