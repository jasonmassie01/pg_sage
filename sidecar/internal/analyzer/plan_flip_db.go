package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/planfresh"
	"github.com/pg-sage/sidecar/internal/planhash"
)

// planFlipReadTimeout bounds each of the rule's reads.
const planFlipReadTimeout = 10 * time.Second

// pgPlanFlipDB reads the plan-flip rule's evidence from the monitored
// database (catalog statistics, sage tables) and its history store.
type pgPlanFlipDB struct {
	pool *pgxpool.Pool
}

// flipRelationsSQL reads the last ANALYZE times of the plan's relations:
// $1 "schema.relation" names, $2 unqualified names (plain EXPLAIN).
const flipRelationsSQL = `/* pg_sage analyzer */
SELECT s.schemaname::text, s.relname::text, s.last_analyze, s.last_autoanalyze
  FROM pg_catalog.pg_stat_user_tables s
 WHERE s.schemaname || '.' || s.relname = ANY($1::text[]) OR s.relname = ANY($2::text[])`

func (d pgPlanFlipDB) relations(ctx context.Context, n planfresh.Names) ([]relStats, error) {
	ctx, cancel := context.WithTimeout(ctx, planFlipReadTimeout)
	defer cancel()
	rows, err := d.pool.Query(ctx, flipRelationsSQL, n.Qualified, n.Unqualified)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []relStats
	for rows.Next() {
		var r relStats
		if err := rows.Scan(&r.Schema, &r.Name, &r.LastAnalyze, &r.LastAutoanalyze); err != nil {
			return nil, fmt.Errorf("scan relation: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// flipSageAnalyzesSQL reads pg_sage's ANALYZE actions of the plan window
// plus the cooldown (through idx_action_log_time), and the database clock.
const flipSageAnalyzesSQL = `/* pg_sage analyzer */
SELECT now(), a.sql_executed, a.executed_at
  FROM (SELECT 1) one
  LEFT JOIN LATERAL (
       SELECT al.sql_executed, al.executed_at FROM sage.action_log al
        WHERE al.executed_at > now() - interval '8 days'
          AND al.action_type = 'analyze' AND al.outcome <> 'failed'
        ORDER BY al.executed_at DESC LIMIT 500) a ON true`

func (d pgPlanFlipDB) sageAnalyzes(ctx context.Context) ([]sageAnalyze, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, planFlipReadTimeout)
	defer cancel()
	rows, err := d.pool.Query(ctx, flipSageAnalyzesSQL)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var out []sageAnalyze
	var now time.Time
	for rows.Next() {
		var sql *string
		var at *time.Time
		if err := rows.Scan(&now, &sql, &at); err != nil {
			return nil, time.Time{}, fmt.Errorf("scan action: %w", err)
		}
		if sql != nil && at != nil {
			out = append(out, sageAnalyze{Target: analyzeTarget(*sql), At: *at})
		}
	}
	return out, now, rows.Err()
}

// flipBlocksSQL reads a query's shared blocks and calls in the 24 hours
// before the hour of $2 and from that hour on.
const flipBlocksSQL = `/* pg_sage analyzer */
SELECT COALESCE(sum(h.shared_blks_hit + h.shared_blks_read)
           FILTER (WHERE h.hour < date_trunc('hour', $2::timestamptz)), 0)::float8,
       COALESCE(sum(h.calls) FILTER (WHERE h.hour < date_trunc('hour', $2::timestamptz)),
           0)::float8,
       COALESCE(sum(h.shared_blks_hit + h.shared_blks_read)
           FILTER (WHERE h.hour >= date_trunc('hour', $2::timestamptz)), 0)::float8,
       COALESCE(sum(h.calls) FILTER (WHERE h.hour >= date_trunc('hour', $2::timestamptz)),
           0)::float8
  FROM sage.query_rollup_hourly h
 WHERE {db:h} AND h.queryid = $1
   AND h.hour >= date_trunc('hour', $2::timestamptz) - interval '24 hours'`

func (d pgPlanFlipDB) blocksPerCall(ctx context.Context, queryid int64,
	pivot time.Time) (blocksPerCall, error) {
	ctx, cancel := context.WithTimeout(ctx, planFlipReadTimeout)
	defer cancel()
	var bb, cb, ba, ca float64
	err := histstore.Resolve(d.pool).QueryRow(ctx, flipBlocksSQL, queryid, pivot).
		Scan(&bb, &cb, &ba, &ca)
	if err != nil {
		return blocksPerCall{}, err
	}
	return blocksPerCall{Before: perCall(bb, cb), After: perCall(ba, ca)}, nil
}

func perCall(blocks, calls float64) *float64 {
	if calls <= 0 {
		return nil
	}
	v := blocks / calls
	return &v
}

// flipOpenSQL reads the open flips ($1 the category) and those resolved
// by pg_sage's ANALYZE in the last 7 days whose verdict is not recorded
// and whose identity has no open row, each with its latest ANALYZE action.
// The two sets are read apart (UNION ALL) so each uses its partial index:
// the open ones idx_findings_dedup, the resolved ones
// idx_findings_resolved_at. One OR over both read every finding (perf
// gate: a seq scan of sage.findings every analyzer cycle).
const flipOpenSQL = `/* pg_sage analyzer */
SELECT f.id, f.object_identifier, f.status, f.severity, f.title,
       COALESCE(f.detail, '{}'::jsonb), al.id, al.executed_at
  FROM (SELECT o.id, o.object_identifier, o.status, o.severity, o.title, o.detail,
               o.action_log_id
          FROM sage.findings o
         WHERE o.category = $1 AND o.status = 'open'
        UNION ALL
        SELECT r.id, r.object_identifier, r.status, r.severity, r.title, r.detail,
               r.action_log_id
          FROM sage.findings r
         WHERE r.category = $1 AND r.status = 'resolved'
           AND r.resolved_at > now() - interval '7 days'
           AND r.action_log_id IS NOT NULL
           AND NOT (COALESCE(r.detail, '{}'::jsonb) ? 'reanalyze_verdict')
           AND NOT EXISTS (SELECT 1 FROM sage.findings o
                            WHERE o.category = r.category AND o.status = 'open'
                              AND o.object_identifier = r.object_identifier)) f
  LEFT JOIN LATERAL (
       SELECT a.id, a.executed_at FROM sage.action_log a
        WHERE a.finding_id = f.id AND a.action_type = 'analyze' AND a.outcome <> 'failed'
        ORDER BY a.executed_at DESC, a.id DESC LIMIT 1) al ON true
 WHERE f.status = 'open' OR (al.id IS NOT NULL AND f.action_log_id = al.id)
 ORDER BY f.id LIMIT 200`

func (d pgPlanFlipDB) openFlips(ctx context.Context) ([]openFlip, error) {
	ctx, cancel := context.WithTimeout(ctx, planFlipReadTimeout)
	defer cancel()
	rows, err := d.pool.Query(ctx, flipOpenSQL, categoryPlanFlip)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []openFlip
	for rows.Next() {
		var o openFlip
		var raw []byte
		var action *int64
		var at *time.Time
		if err := rows.Scan(&o.ID, &o.Ident, &o.Status, &o.Severity, &o.Title, &raw,
			&action, &at); err != nil {
			return nil, fmt.Errorf("scan flip: %w", err)
		}
		detail, err := decodeFlipDetail(raw)
		if err != nil {
			return nil, fmt.Errorf("decode the detail of finding %d: %w", o.ID, err)
		}
		o.Detail = detail
		if action != nil && at != nil {
			o.ActionID, o.ActionAt = *action, *at
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

const flipLatestSQL = `/* pg_sage analyzer */
SELECT e.plan_json, e.captured_at FROM sage.explain_cache e
 WHERE e.queryid = $1 AND e.source <> 'lakebase_plan_history'
 ORDER BY e.captured_at DESC, e.id DESC LIMIT 1`

// latestCapture fingerprints the newest plan as the pair rule does (roots
// resolved; an unresolved v2u hash is never compared with a v2 one).
func (d pgPlanFlipDB) latestCapture(ctx context.Context, queryid int64) (*latestCapture,
	error) {
	ctx, cancel := context.WithTimeout(ctx, planFlipReadTimeout)
	defer cancel()
	var plan []byte
	var at time.Time
	err := d.pool.QueryRow(ctx, flipLatestSQL, queryid).Scan(&plan, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	roots, _ := planhash.Resolve(ctx, d.pool, plan)
	hash, err := planhash.ComputeWith(plan, planhash.Options{Roots: roots})
	if err != nil {
		return &latestCapture{At: at}, nil
	}
	return &latestCapture{Hash: hash, At: at}, nil
}

func (d pgPlanFlipDB) recordVerdict(ctx context.Context, id int64,
	detail map[string]any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encode the verdict: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, planFlipReadTimeout)
	defer cancel()
	_, err = d.pool.Exec(ctx, `/* pg_sage analyzer */ UPDATE sage.findings
		SET detail = COALESCE(detail, '{}'::jsonb) || $2::jsonb WHERE id = $1`, id, raw)
	return err
}
