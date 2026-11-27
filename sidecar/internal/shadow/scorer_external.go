package shadow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// A change pg_sage wanted that someone applied outside pg_sage (a
// migration, an operator in psql) is found in the catalog: an index of
// the same shape on the table, or the index pg_sage wanted to drop gone.
// It happened after pg_sage last proposed it (applied_after) and before
// the pass that found it (applied_detected_at); the targeted queries are
// then verified like pg_sage's own actions: call-weighted, variance-aware
// before/after windows around that gap.

// indexExternal is the verification of an index create or drop applied
// outside pg_sage; nil when nothing changed. own is true when the change
// is in the catalog but pg_sage made it (or may have): no other source
// may score it then, not even the what-if of an index that already
// exists, and the horizon leaves it unscored. A
// change already marked applied is asked again on every pass: pg_sage's
// action may be logged just after its index became valid, and a mark may
// predate this check (an upgrade).
func (s *Scorer) indexExternal(ctx context.Context, p pendingDecision) (*verifiedFact, bool,
	error) {
	marked := p.AppliedDetectedAt != nil && p.AppliedAfter != nil
	if !marked {
		applied, err := s.appliedOutside(ctx, p.Decision)
		if err != nil || !applied {
			return nil, false, err
		}
	}
	own, err := s.madeByPgSage(ctx, p.Decision) // scorer_own.go
	if err != nil || own {
		return nil, own, err
	}
	if !marked {
		if err := s.markApplied(ctx, p.ID); err != nil {
			return nil, false, err
		}
		return &verifiedFact{Reason: "the after window has just started"}, false, nil
	}
	f, err := s.verifyApplied(ctx, p)
	return f, false, err
}

// appliedOutside checks the catalog for d's change.
func (s *Scorer) appliedOutside(ctx context.Context, d Decision) (bool, error) {
	if d.Class == "index_drop" {
		return s.droppedOutside(ctx, d)
	}
	table, ok := shapeTable(d.Shape)
	if !ok {
		return false, nil
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT pg_get_indexdef(i.indexrelid)
		FROM pg_index i WHERE i.indrelid = to_regclass($1) AND i.indisvalid`, table)
	if err != nil {
		return false, fmt.Errorf("shadow: read indexes of %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			return false, fmt.Errorf("shadow: scan index of %s: %w", table, err)
		}
		if Shape(def) == d.Shape {
			return true, nil
		}
	}
	return false, rows.Err()
}

// droppedOutside: the index d drops is gone and its table is not. The
// table is named by d's rollback (the index definition); without it a
// dropped table cannot be ruled out, and the drop is not counted.
func (s *Scorer) droppedOutside(ctx context.Context, d Decision) (bool, error) {
	name := strings.TrimPrefix(d.Shape, "drop index ")
	table, ok := shapeTable(Shape(d.RollbackSQL))
	if !ok {
		return false, nil
	}
	var dropped bool
	if err := s.pool.QueryRow(ctx, `/* pg_sage */ SELECT to_regclass($1) IS NULL
		AND to_regclass($2) IS NOT NULL`, name, table).Scan(&dropped); err != nil {
		return false, fmt.Errorf("shadow: look up index %s of %s: %w", name, table, err)
	}
	return dropped, nil
}

// shapeTable is the table of an index-create shape.
func shapeTable(shape string) (string, bool) {
	_, rest, ok := strings.Cut(shape, " on ")
	if !ok {
		return "", false
	}
	table, _, ok := strings.Cut(rest, " using ")
	return table, ok && table != ""
}

// markAppliedSQL notes when a pending decision's change was found applied
// outside pg_sage. It sets only unindexed columns, so it stays HOT.
const markAppliedSQL = `/* pg_sage */ UPDATE sage.shadow_decision
	SET applied_after = last_seen_at, applied_detected_at = now()
	WHERE id = $1 AND status = 'pending' AND applied_detected_at IS NULL`

func (s *Scorer) markApplied(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx, markAppliedSQL, id); err != nil {
		return fmt.Errorf("shadow: mark decision %d applied: %w", id, err)
	}
	return nil
}

// verifyApplied compares the targets before the gap with after it (an
// extended statistics object: with its row estimates too). The
// after window grows with time from VerifyWindow to its cap; a
// regression is final at once, an improvement or neutral once the
// class's minimum window (a drop's business cycle) elapsed, too little
// evidence only at the cap.
func (s *Scorer) verifyApplied(ctx context.Context, p pendingDecision) (*verifiedFact, error) {
	minWin, maxWin := s.opts.VerifyWindow, s.opts.VerifyMaxWindow
	if p.Class == "index_drop" {
		minWin, maxWin = s.opts.DropWindow, max(s.opts.DropWindow, s.opts.VerifyMaxWindow)
	}
	elapsed := p.now.Sub(*p.AppliedDetectedAt)
	if elapsed < s.opts.VerifyWindow {
		return &verifiedFact{Reason: "the after window is still open"}, nil
	}
	ids := p.Prediction.TargetQueryIDs
	if len(ids) == 0 {
		return &verifiedFact{Verdict: verify.OutcomeUnverifiable, Final: true,
			Reason: "the decision named no targeted queries to verify"}, nil
	}
	win := min(elapsed, maxWin)
	source := verify.NewPostgresObservationSource(s.pool)
	before, err := source.QueryMeasurements(ctx, ids, p.AppliedAfter.Add(-win), *p.AppliedAfter)
	if err != nil {
		return nil, fmt.Errorf("shadow: measure before decision %d: %w", p.ID, err)
	}
	after, err := source.QueryMeasurements(ctx, ids, *p.AppliedDetectedAt,
		p.AppliedDetectedAt.Add(win))
	if err != nil {
		return nil, fmt.Errorf("shadow: measure after decision %d: %w", p.ID, err)
	}
	pooled, per := verify.DecideTargets(before, after, ids, s.opts.Thresholds)
	f := &verifiedFact{Verdict: pooled.Verdict, Reason: pooled.Reason,
		Evidence: map[string]any{"verdict": pooled.Verdict, "comparison": pooled.Evidence(),
			"targets": verify.TargetsEvidence(per), "window_seconds": win.Seconds(),
			"applied_after": p.AppliedAfter, "applied_detected_at": p.AppliedDetectedAt}}
	if p.Class == "statistics" { // scorer_config.go
		if err := s.decideStatistics(ctx, p, f, &pooled, win); err != nil {
			return nil, err
		}
	}
	f.Final = finalVerdict(f.Verdict, win, minWin, maxWin)
	return f, nil
}

func finalVerdict(verdict string, win, minWin, maxWin time.Duration) bool {
	switch verdict {
	case verify.OutcomeRegressed:
		return true
	case verify.OutcomeImproved, verify.OutcomeNeutral:
		return win >= minWin
	}
	return win >= maxWin
}
