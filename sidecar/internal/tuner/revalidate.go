package tuner

import (
	"context"
	"fmt"
	"time"
)

// RevalidateReport summarizes one pass of the hint revalidation
// loop. Fields are per-cycle counters suitable for logging and
// Prometheus metrics.
type RevalidateReport struct {
	Checked     int
	Kept        int
	Dormant     int
	Reset       int
	ReverifyDue int
	Broken      int
	Retired     int
	Errors      int
	Duration    time.Duration
}

// hintRow is the subset of sage.query_hints columns the
// revalidator reads.
type hintRow struct {
	ID               int64
	QueryID          int64
	Status           string
	HintText         string
	CreatedAt        time.Time
	LastRevalidated  *time.Time
	CallsAtLastCheck *int64
	PgssResetAt      *time.Time
	ReverifyDue      *time.Time
}

// Revalidate runs one pass of the hint revalidation loop over active and
// dormant hints and writes each decision back to sage.query_hints. Safe to
// call concurrently with Tune(); the Tuner mutex serializes both.
func (t *Tuner) Revalidate(ctx context.Context) (RevalidateReport, error) {
	start := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	var report RevalidateReport
	if t.pool == nil {
		return report, nil
	}

	hints, err := t.loadHintsForRevalidation(ctx)
	if err != nil {
		return report, fmt.Errorf("load hints for revalidation: %w", err)
	}
	report.Checked = len(hints)
	retentionCutoff := time.Duration(
		t.cfg.HintRetirementDays) * 24 * time.Hour

	for _, h := range hints {
		decision := t.reverifyHint(ctx, h, t.decideHintFate(ctx, h, retentionCutoff))
		if err := t.recordRevalidation(ctx, h, decision); err != nil {
			decision = hintDecision{action: hintActionError, reason: fmt.Sprintf(
				"record %s: %v", decision.nextStatus(h), err)}
		}
		report.count(decision)
		t.logDecision(h, decision)
	}
	report.Duration = time.Since(start)
	return report, nil
}

func (r *RevalidateReport) count(d hintDecision) {
	switch d.action {
	case hintActionKeep:
		r.Kept++
	case hintActionDormant:
		r.Dormant++
	case hintActionReset:
		r.Reset++
	case hintActionBroken:
		r.Broken++
	case hintActionRetire:
		r.Retired++
	case hintActionError:
		r.Errors++
		return
	}
	if d.reverifyDue {
		r.ReverifyDue++
	}
}

func (t *Tuner) logDecision(h hintRow, d hintDecision) {
	if d.action == hintActionError {
		t.logFn("WARN", "revalidate: hint %d left %s: %s", h.ID, h.Status, d.reason)
		return
	}
	if next := d.nextStatus(h); next != h.Status || d.action == hintActionReset {
		t.logFn("INFO", "revalidate: hint %d %s -> %s (%s)",
			h.ID, h.Status, next, d.reason)
	}
	if d.reverifyDue {
		t.logFn("INFO", "revalidate: hint %d due for re-verification "+
			"(age_reverify: older than %d days)", h.ID, t.cfg.HintRetirementDays)
	}
}

// decideHintFate runs the revalidation checks for one hint:
//  1. Object existence: parse the directive and verify referenced
//     indexes still exist in pg_class. A dropped index => broken.
//  2. Statistics: read pg_stat_statements (summed over the queryid's
//     rows in this database) and pg_stat_statements_info.stats_reset,
//     then decideFromObservation: reset, dormant or active, and whether
//     the hint is due for re-verification by age.
//
// A statistics read error is hintActionError and leaves the hint as it is.
func (t *Tuner) decideHintFate(
	ctx context.Context,
	h hintRow,
	retentionCutoff time.Duration,
) hintDecision {
	// Check 1: object existence. Unparseable directives are logged but
	// NOT marked broken per plan §3.3 — parser failure is our bug, not
	// the hint's.
	if h.HintText != "" {
		if missing := t.checkHintObjectExistence(ctx, h.HintText); missing != "" {
			return hintDecision{
				action: hintActionBroken,
				reason: fmt.Sprintf(
					"referenced object missing from catalog: %s",
					missing),
			}
		}
	}
	obs, err := t.fetchQueryStats(ctx, h.QueryID)
	if err != nil {
		return hintDecision{
			action: hintActionError,
			reason: fmt.Sprintf("fetch stats: %v", err),
		}
	}
	return decideFromObservation(h, obs, retentionCutoff, time.Now())
}

func (t *Tuner) loadHintsForRevalidation(
	ctx context.Context,
) ([]hintRow, error) {
	rows, err := t.pool.Query(ctx, `/* pg_sage tuner */
SELECT id, queryid, status, COALESCE(hint_text, ''), created_at,
       last_revalidated_at, calls_at_last_check, pgss_reset_at, reverify_due
FROM sage.query_hints
WHERE status IN ('active', 'dormant')
ORDER BY last_revalidated_at NULLS FIRST, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hintRow
	for rows.Next() {
		var h hintRow
		if err := rows.Scan(
			&h.ID, &h.QueryID, &h.Status, &h.HintText,
			&h.CreatedAt, &h.LastRevalidated, &h.CallsAtLastCheck,
			&h.PgssResetAt, &h.ReverifyDue,
		); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// queryStatsSQL sums the queryid's pg_stat_statements rows in this
// database. pg_stat_statements keeps one row per (userid, dbid, queryid,
// toplevel), so one row (the old LIMIT 1) could change between passes when
// two roles run the statement. The same queryid hash can also appear in
// other databases, hence the dbid scope. A NULL sum means not found.
// view and info are the qualified pg_stat_statements views.
func queryStatsSQL(view, info string) string {
	return `/* pg_sage tuner */
SELECT (SELECT stats_reset FROM ` + info + `),
       sum(calls)::bigint,
       sum(total_exec_time) / NULLIF(sum(calls), 0)
FROM ` + view + `
WHERE queryid = $1
  AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`
}

func (t *Tuner) fetchQueryStats(
	ctx context.Context, queryID int64,
) (pgssObservation, error) {
	var obs pgssObservation
	if t.pool == nil {
		return obs, fmt.Errorf(
			"read pg_stat_statements for queryid %d: no connection pool", queryID)
	}
	var calls *int64
	var meanMs *float64
	view, info, err := statementsViews(ctx, t.pool)
	if err == nil {
		err = t.pool.QueryRow(ctx, queryStatsSQL(view, info), queryID).Scan(
			&obs.ResetAt, &calls, &meanMs)
	}
	if err != nil {
		return obs, fmt.Errorf("read pg_stat_statements and "+
			"pg_stat_statements_info for queryid %d: %w", queryID, err)
	}
	if calls == nil {
		return obs, nil
	}
	obs.Calls, obs.Found = *calls, true
	if meanMs != nil {
		obs.MeanMs = *meanMs
	}
	return obs, nil
}

// checkHintObjectExistence parses the hint text and verifies that
// every index named in an IndexScan / BitmapScan directive still
// exists in pg_class. Returns the first missing index name found,
// or "" if all referenced objects are present (or the hint has no
// catalog-checkable references).
//
// Unparseable directives do NOT produce a missing-object result;
// per plan §3.3 Check 1 they are logged and skipped.
func (t *Tuner) checkHintObjectExistence(
	ctx context.Context, hintText string,
) string {
	if t.pool == nil || hintText == "" {
		return ""
	}
	parsed, unparseable := ParseHintText(hintText)
	if HasUnparseable(unparseable) {
		t.logFn("INFO",
			"revalidate: hint_unparseable directives=%v", unparseable)
	}
	for _, d := range parsed {
		if d.Kind != "IndexScan" && d.Kind != "BitmapScan" {
			continue
		}
		for _, idx := range d.IndexNames {
			exists, err := t.indexExists(ctx, idx)
			if err != nil {
				// Catalog read error — treat as transient and skip
				// (do NOT mark broken on our own failure).
				t.logFn("WARN",
					"revalidate: index existence check for %q: %v",
					idx, err)
				continue
			}
			if !exists {
				return idx
			}
		}
	}
	return ""
}

// indexExists returns true if a relation named idxName exists in
// pg_class with relkind='i' (index) or 'I' (partitioned index).
// The lookup is unqualified because pg_hint_plan itself accepts
// unqualified index names; catalog search_path resolves them.
func (t *Tuner) indexExists(
	ctx context.Context, idxName string,
) (bool, error) {
	var n int
	err := t.pool.QueryRow(ctx, `/* pg_sage tuner */ 
SELECT count(*)
FROM pg_class
WHERE relname = $1
  AND relkind IN ('i', 'I')`, idxName).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// recordRevalidationSQL writes one decision. Only live (active or dormant)
// rows are written, so a hint reconciled to rolled_back meanwhile stays so.
const recordRevalidationSQL = `/* pg_sage tuner */
UPDATE sage.query_hints
SET status = $2,
    rolled_back_at = CASE WHEN $2 = 'broken' THEN now() ELSE rolled_back_at END,
    calls_at_last_check = COALESCE($3, calls_at_last_check),
    pgss_reset_at = COALESCE($4, pgss_reset_at),
    reverify_due = CASE WHEN $5 THEN COALESCE(reverify_due, now())
                        ELSE reverify_due END,
    last_revalidated_at = now()
WHERE id = $1 AND status IN ('active', 'dormant')`

// recordRevalidation writes the decision's status, checkpoint, stats_reset
// and reverify_due. An error decision writes nothing.
func (t *Tuner) recordRevalidation(
	ctx context.Context, h hintRow, d hintDecision,
) error {
	if d.action == hintActionError {
		return nil
	}
	_, err := t.pool.Exec(ctx, recordRevalidationSQL,
		h.ID, d.nextStatus(h), d.checkpoint, d.resetAt, d.reverifyDue)
	if err != nil {
		return fmt.Errorf("update sage.query_hints id %d: %w", h.ID, err)
	}
	return nil
}

// StartRevalidationLoop runs the revalidation loop on a ticker
// until ctx is cancelled. intervalHours <= 0 disables the loop.
func (t *Tuner) StartRevalidationLoop(
	ctx context.Context, intervalHours int,
) {
	if !t.cfg.VerifyAfterApply {
		// C12: verify_after_apply is documented as the switch for this
		// loop; it was previously read nowhere.
		t.logFn("INFO",
			"revalidate: loop disabled (tuner.verify_after_apply=false)")
		return
	}
	if intervalHours <= 0 {
		t.logFn("INFO",
			"revalidate: loop disabled (interval_hours=%d)",
			intervalHours)
		return
	}
	d := time.Duration(intervalHours) * time.Hour
	t.logFn("INFO",
		"revalidate: loop starting, interval=%s", d)
	ticker := time.NewTicker(d)
	defer ticker.Stop()

	// Run once immediately so operators don't need to wait a
	// full interval before the first pass.
	t.revalidateAndLog(ctx, "initial pass")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.revalidateAndLog(ctx, "pass")
		}
	}
}

func (t *Tuner) revalidateAndLog(ctx context.Context, label string) {
	rpt, err := t.Revalidate(ctx)
	if err != nil {
		t.logFn("WARN", "revalidate: %s error: %v", label, err)
		return
	}
	t.logFn("INFO",
		"revalidate: %s checked=%d kept=%d dormant=%d reset=%d "+
			"reverify_due=%d broken=%d retired=%d errors=%d in %s",
		label, rpt.Checked, rpt.Kept, rpt.Dormant, rpt.Reset,
		rpt.ReverifyDue, rpt.Broken, rpt.Retired, rpt.Errors, rpt.Duration)
}
