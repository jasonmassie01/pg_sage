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

// external is the verification of d's change applied outside pg_sage;
// nil when the class cannot be checked in the catalog or nothing changed.
func (s *Scorer) external(ctx context.Context, p pendingDecision) (*verifiedFact, error) {
	if p.Class != "index_create" && p.Class != "index_drop" {
		return nil, nil
	}
	if p.AppliedDetectedAt == nil || p.AppliedAfter == nil {
		applied, err := s.appliedOutside(ctx, p.Decision)
		if err != nil || !applied {
			return nil, err
		}
		if err := s.markApplied(ctx, p.ID); err != nil {
			return nil, err
		}
		return &verifiedFact{Reason: "the after window has just started"}, nil
	}
	return s.verifyApplied(ctx, p)
}

// appliedOutside checks the catalog for d's change.
func (s *Scorer) appliedOutside(ctx context.Context, d Decision) (bool, error) {
	if d.Class == "index_drop" {
		name := strings.TrimPrefix(d.Shape, "drop index ")
		var gone bool
		if err := s.pool.QueryRow(ctx, `/* pg_sage */ SELECT to_regclass($1) IS NULL`,
			name).Scan(&gone); err != nil {
			return false, fmt.Errorf("shadow: look up index %s: %w", name, err)
		}
		return gone, nil
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

// shapeTable is the table of an index-create shape.
func shapeTable(shape string) (string, bool) {
	_, rest, ok := strings.Cut(shape, " on ")
	if !ok {
		return "", false
	}
	table, _, ok := strings.Cut(rest, " using ")
	return table, ok && table != ""
}

func (s *Scorer) markApplied(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.shadow_decision
		SET applied_after = last_seen_at, applied_detected_at = now()
		WHERE id = $1 AND status = 'pending' AND applied_detected_at IS NULL`, id); err != nil {
		return fmt.Errorf("shadow: mark decision %d applied: %w", id, err)
	}
	return nil
}

// verifyApplied compares the targets before the gap with after it. The
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
	f.Final = finalVerdict(pooled.Verdict, win, minWin, maxWin)
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
