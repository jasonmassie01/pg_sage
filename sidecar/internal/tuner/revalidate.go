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
	Checked  int
	Kept     int
	Retired  int
	Broken   int
	Errors   int
	Duration time.Duration
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
}

// Revalidate runs one pass of the hint revalidation loop and
// writes status changes back to sage.query_hints. Safe to call
// concurrently with Tune(); the Tuner mutex serializes both.
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
		return report, fmt.Errorf("load active hints: %w", err)
	}
	report.Checked = len(hints)
	retentionCutoff := time.Duration(
		t.cfg.HintRetirementDays) * 24 * time.Hour

	for _, h := range hints {
		decision := t.decideHintFate(ctx, h, retentionCutoff)
		switch decision.action {
		case hintActionKeep:
			report.Kept++
		case hintActionRetired:
			report.Retired++
			t.updateHintStatus(ctx, h.ID, "retired", decision.reason)
		case hintActionBroken:
			report.Broken++
			t.updateHintStatus(ctx, h.ID, "broken", decision.reason)
		case hintActionError:
			report.Errors++
		}
		t.updateRevalidationTimestamp(ctx, h.ID, decision.currentCalls)
	}
	report.Duration = time.Since(start)
	return report, nil
}

type hintAction int

const (
	hintActionKeep hintAction = iota
	hintActionRetired
	hintActionBroken
	hintActionError
)

type hintDecision struct {
	action       hintAction
	reason       string
	currentCalls int64
}

// decideHintFate runs the checks from plan_v0.8.5.md §3 in order,
// short-circuiting on the first actionable outcome:
//  0. Age cutoff — retire hints older than HintRetirementDays
//  1. Object existence — parse directive, verify referenced
//     indexes still exist in pg_class. Dropped index ⇒ broken.
//  2. Dead queryid — missing from pg_stat_statements ⇒ broken
//  3. Stagnant calls — calls_at_last_check unchanged ⇒ broken
//  4. Observational success — mean_exec_time dropped ⇒ retired
func (t *Tuner) decideHintFate(
	ctx context.Context,
	h hintRow,
	retentionCutoff time.Duration,
) hintDecision {
	// Check 0: age cutoff.
	if retentionCutoff > 0 && time.Since(h.CreatedAt) > retentionCutoff {
		return hintDecision{
			action: hintActionRetired,
			reason: fmt.Sprintf(
				"retention cutoff exceeded (%s old)",
				time.Since(h.CreatedAt).Round(time.Hour),
			),
		}
	}

	// Check 1: object existence — parse the directive and look up
	// every referenced index in pg_class. Unparseable directives
	// are logged but NOT marked broken per plan §3.3 — parser
	// failure is our bug, not the hint's.
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

	// Check 2 + 3: fetch current stats for queryid.
	currentCalls, meanMs, found, err := t.fetchQueryStats(ctx, h.QueryID)
	if err != nil {
		return hintDecision{
			action: hintActionError,
			reason: fmt.Sprintf("fetch stats: %v", err),
		}
	}
	if !found {
		return hintDecision{
			action: hintActionBroken,
			reason: "queryid absent from pg_stat_statements",
		}
	}
	// Check 3: stagnant calls between revalidation passes.
	if h.CallsAtLastCheck != nil && currentCalls <= *h.CallsAtLastCheck {
		return hintDecision{
			action:       hintActionBroken,
			reason:       "no new executions since last revalidation",
			currentCalls: currentCalls,
		}
	}
	// Check 4: observational success. If mean execution time has
	// dropped below 100 ms the underlying issue has resolved
	// itself (typically via an ANALYZE or index landing) and the
	// hint is no longer pulling its weight.
	if meanMs > 0 && meanMs < 100 {
		return hintDecision{
			action: hintActionRetired,
			reason: fmt.Sprintf(
				"observational success: mean_exec_time %.1f ms", meanMs,
			),
			currentCalls: currentCalls,
		}
	}
	return hintDecision{
		action:       hintActionKeep,
		currentCalls: currentCalls,
	}
}

func (t *Tuner) loadHintsForRevalidation(
	ctx context.Context,
) ([]hintRow, error) {
	rows, err := t.pool.Query(ctx, `
SELECT id, queryid, status, COALESCE(hint_text, ''), created_at,
       last_revalidated_at, calls_at_last_check
FROM sage.query_hints
WHERE status = 'active'
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
		); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (t *Tuner) fetchQueryStats(
	ctx context.Context, queryID int64,
) (calls int64, meanMs float64, found bool, err error) {
	// pg_stat_statements is server-wide: the same queryid hash can
	// legitimately appear in multiple databases. We must scope the
	// lookup to the current database or we risk reading stats for a
	// neighbouring DB that happens to share a queryid. dbid is the
	// pg_database OID of the database where the statement ran.
	err = t.pool.QueryRow(ctx, `
SELECT calls, mean_exec_time
FROM pg_stat_statements
WHERE queryid = $1
  AND dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
LIMIT 1`, queryID,
	).Scan(&calls, &meanMs)
	if err == nil {
		return calls, meanMs, true, nil
	}
	// pgx returns ErrNoRows for missing rows; treat as not-found.
	if isNoRowsErr(err) {
		return 0, 0, false, nil
	}
	return 0, 0, false, err
}

func isNoRowsErr(err error) bool {
	if err == nil {
		return false
	}
	// pgx.ErrNoRows satisfies this check via string match; a
	// type switch would introduce a pgx import here for a
	// purely cosmetic check.
	return err.Error() == "no rows in result set"
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
	err := t.pool.QueryRow(ctx, `
SELECT count(*)
FROM pg_class
WHERE relname = $1
  AND relkind IN ('i', 'I')`, idxName).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (t *Tuner) updateHintStatus(
	ctx context.Context, hintID int64, status, reason string,
) {
	_, err := t.pool.Exec(ctx, `
UPDATE sage.query_hints
SET status = $2,
    rolled_back_at = CASE WHEN $2 = 'broken'
                          THEN now() ELSE rolled_back_at END,
    verified_at  = CASE WHEN $2 = 'retired'
                        THEN now() ELSE verified_at END,
    last_revalidated_at = now()
WHERE id = $1`, hintID, status)
	if err != nil {
		t.logFn("WARN",
			"revalidate: update hint %d → %s: %v",
			hintID, status, err)
		return
	}
	t.logFn("INFO",
		"revalidate: hint %d → %s (%s)", hintID, status, reason)
}

func (t *Tuner) updateRevalidationTimestamp(
	ctx context.Context, hintID int64, calls int64,
) {
	// When calls is 0 the hint was marked broken/retired and
	// the status update already touched last_revalidated_at.
	if calls <= 0 {
		return
	}
	_, err := t.pool.Exec(ctx, `
UPDATE sage.query_hints
SET last_revalidated_at = now(),
    calls_at_last_check = $2
WHERE id = $1 AND status = 'active'`, hintID, calls)
	if err != nil {
		t.logFn("WARN",
			"revalidate: update timestamps for hint %d: %v",
			hintID, err)
	}
}

// StartRevalidationLoop runs the revalidation loop on a ticker
// until ctx is cancelled. intervalHours <= 0 disables the loop.
func (t *Tuner) StartRevalidationLoop(
	ctx context.Context, intervalHours int,
) {
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
	if rpt, err := t.Revalidate(ctx); err != nil {
		t.logFn("WARN", "revalidate: initial pass: %v", err)
	} else {
		t.logFn("INFO",
			"revalidate: initial pass checked=%d kept=%d "+
				"retired=%d broken=%d errors=%d in %s",
			rpt.Checked, rpt.Kept, rpt.Retired,
			rpt.Broken, rpt.Errors, rpt.Duration)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rpt, err := t.Revalidate(ctx)
			if err != nil {
				t.logFn("WARN", "revalidate: pass error: %v", err)
				continue
			}
			t.logFn("INFO",
				"revalidate: pass checked=%d kept=%d "+
					"retired=%d broken=%d errors=%d in %s",
				rpt.Checked, rpt.Kept, rpt.Retired,
				rpt.Broken, rpt.Errors, rpt.Duration)
		}
	}
}
