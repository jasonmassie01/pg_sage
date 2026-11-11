package tuner

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HintQuerier is the part of a pool or connection the readiness probe uses.
type HintQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DetectHintPlan reads whether pg_hint_plan hints change application plans
// (ProbeHintReadiness) and, when the module is not in
// shared_preload_libraries, whether LOAD works in pg_sage's own backend.
// That LOAD serves only pg_sage's own hinted EXPLAIN (SessionLoad); it never
// gives application effect. The result is never nil; the error, when set,
// names the read that failed and the result is then not ready.
func DetectHintPlan(
	ctx context.Context,
	pool *pgxpool.Pool,
) (*HintPlanAvailability, error) {
	if pool == nil {
		return unavailableHintPlan(), nil
	}
	avail, err := ProbeHintReadiness(ctx, pool)
	if avail.SharedPreload {
		return avail, err
	}
	session, loadErr := checkHintSessionLoad(ctx, pool)
	withSessionLoad(avail, session)
	if loadErr != nil {
		err = errors.Join(err, fmt.Errorf("detect pg_hint_plan: %w", loadErr))
	}
	return avail, err
}

// unavailableHintPlan is the result without a connection: nothing read,
// nothing ready, and nothing to re-read (CheckedAt stays zero).
func unavailableHintPlan() *HintPlanAvailability {
	return &HintPlanAvailability{
		Method:  "unavailable",
		Reasons: []string{reasonNoConnection},
	}
}

// withSessionLoad records whether LOAD 'pg_hint_plan' worked in pg_sage's
// own backend. It changes Method only, never ApplicationEffect.
func withSessionLoad(a *HintPlanAvailability, loaded bool) {
	a.SessionLoad = loaded
	a.Method = hintMethod(a)
}

// hintMethod names how pg_hint_plan reaches sessions: preloaded for every
// session, preloaded per session, loaded only into pg_sage, or not at all.
func hintMethod(a *HintPlanAvailability) string {
	switch {
	case a.SharedPreload:
		return "shared_preload"
	case a.Preload:
		return "session_preload"
	case a.SessionLoad:
		return "session_load"
	}
	return "unavailable"
}

// checkHintSessionLoad attempts LOAD 'pg_hint_plan' on a
// single connection. Permission errors are not propagated.
func checkHintSessionLoad(
	ctx context.Context,
	pool *pgxpool.Pool,
) (bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf(
			"acquire connection: %w", err,
		)
	}
	defer conn.Release()

	_, err = conn.Exec(ctx, "LOAD 'pg_hint_plan'")
	if err != nil {
		return false, nil
	}
	return true, nil
}

// checkHintTable reports whether hint_plan.hints has the pg_hint_plan 1.7+
// layout the tuner writes (query_id, application_name, hints) and pg_sage
// can read it. pg_hint_plan 1.4-1.6, shipped for PG14-16, key hints by
// norm_query_string; that table is not ready, so tuning findings stay
// advisory (no executable SQL).
//
// The table also needs a valid, non-partial unique index on columns among
// (query_id, application_name), as pg_hint_plan 1.7 ships: the apply INSERT
// relies on it to fail over an existing row (tuning-02). Without it hints
// stay advisory too.
func checkHintTable(ctx context.Context, q HintQuerier) bool {
	layout, readable, _, err := readHintTable(ctx, q)
	return err == nil && layout && readable
}

// readHintTable reads the layout and pg_sage's privileges on
// hint_plan.hints from the catalog alone, so a missing table or schema
// privilege is a fact, not an error.
func readHintTable(
	ctx context.Context, q HintQuerier,
) (layout, readable, writable bool, err error) {
	err = q.QueryRow(ctx, hintTableSQL).Scan(&layout, &readable, &writable)
	if err != nil {
		return false, false, false, fmt.Errorf("read hint_plan.hints layout: %w", err)
	}
	return layout, readable, writable, nil
}

// hintTableSQL reads three facts about hint_plan.hints: the 1.7 layout
// (query_id, application_name and hints columns plus a valid unique index,
// without predicate or expressions, whose every column is query_id or
// application_name, so two rows for one (query_id, application_name)
// cannot coexist); whether pg_sage can read it (USAGE on the schema and
// SELECT); and whether it can write it (INSERT and DELETE, each checked on
// its own: has_table_privilege with a list is true when any one is held).
const hintTableSQL = `/* pg_sage tuner */
WITH h AS (
  SELECT c.oid, n.oid AS nsp FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = 'hint_plan' AND c.relname = 'hints'
     AND c.relkind IN ('r', 'p'))
SELECT
  coalesce((SELECT (SELECT count(*) FROM pg_catalog.pg_attribute a
                     WHERE a.attrelid = h.oid AND a.attnum > 0 AND NOT a.attisdropped
                       AND a.attname IN ('query_id', 'application_name', 'hints')) = 3
               AND EXISTS (
                 SELECT 1 FROM pg_catalog.pg_index i
                  WHERE i.indrelid = h.oid
                    AND i.indisunique AND i.indisvalid
                    AND i.indpred IS NULL AND i.indexprs IS NULL
                    AND NOT EXISTS (
                      SELECT 1 FROM pg_catalog.pg_attribute a
                       WHERE a.attrelid = i.indrelid
                         AND a.attnum = ANY (i.indkey::int2[])
                         AND a.attname NOT IN ('query_id', 'application_name')))
              FROM h), false),
  coalesce((SELECT has_schema_privilege(h.nsp, 'USAGE')
               AND has_table_privilege(h.oid, 'SELECT') FROM h), false),
  coalesce((SELECT has_table_privilege(h.oid, 'INSERT')
               AND has_table_privilege(h.oid, 'DELETE') FROM h), false)`
