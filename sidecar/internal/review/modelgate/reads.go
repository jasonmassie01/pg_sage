package modelgate

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/modellift"
)

// Pending is the live G-M1 promotion waiting for a person: the newest G-M1
// row, when it is pending and unapproved. At most one item.
func (g *Gate) Pending(ctx context.Context) ([]Row, error) {
	if g == nil || g.db == nil {
		return nil, ErrNoStore
	}
	return queryRows(ctx, g.db, "the pending G-M1 promotion", `/* pg_sage */
		SELECT `+rowColumns+` FROM sage.review_gate
		WHERE rule = 'G-M1' AND verdict = 'pending_approval' AND approved_at IS NULL
		  AND id = (SELECT max(id) FROM sage.review_gate WHERE rule = 'G-M1')`)
}

// Latest is the newest row of each rule (G-M1 first): the gate status a
// scorecard shows.
func (g *Gate) Latest(ctx context.Context) ([]Row, error) {
	if g == nil || g.db == nil {
		return nil, ErrNoStore
	}
	return queryRows(ctx, g.db, "the latest gate rows", `/* pg_sage */
		SELECT DISTINCT ON (rule) `+rowColumns+` FROM sage.review_gate
		ORDER BY rule, id DESC`)
}

// ModelPassDropped reports whether G-M2 dropped the model pass for model,
// with why. A model with no G-M2 row (a new model id) is not dropped.
func (g *Gate) ModelPassDropped(ctx context.Context, model string) (bool, string, error) {
	if g == nil || g.db == nil {
		return false, "", ErrNoStore
	}
	m := modellift.NormalizeModel(model)
	if m == "" {
		return false, "", nil
	}
	row, err := latest(ctx, g.db, m, RuleM2)
	if err != nil || row == nil || row.Verdict != StateModelOff {
		return false, "", err
	}
	return true, row.Reason, nil
}

// Notes are the gate's changes in [since, until), oldest first, one digest
// line each: a grant, a revocation, and G-M2 dropping or restoring the
// model pass.
func (g *Gate) Notes(ctx context.Context, since, until time.Time) ([]string, error) {
	if g == nil || g.db == nil {
		return nil, ErrNoStore
	}
	rows, err := queryRows(ctx, g.db, "the gate's changes", `/* pg_sage */
		WITH g AS (SELECT *, lag(verdict) OVER (PARTITION BY model, rule ORDER BY id)
			AS prev FROM sage.review_gate)
		SELECT `+rowColumns+` FROM g
		WHERE (verdict = 'revoked' AND evaluated_at >= $1 AND evaluated_at < $2)
		   OR (verdict = 'granted' AND prev IS DISTINCT FROM 'granted'
		       AND approved_at >= $1 AND approved_at < $2)
		   OR (rule = 'G-M2' AND evaluated_at >= $1 AND evaluated_at < $2
		       AND ((prev IS NULL AND verdict = 'model_off')
		         OR (prev IS NOT NULL AND prev <> verdict)))
		ORDER BY COALESCE(approved_at, evaluated_at), id`, since, until)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, note(r))
	}
	return out, nil
}

func note(r Row) string {
	switch r.Verdict {
	case StateGranted:
		return fmt.Sprintf("G-M1 granted for model %s by %s: its steps now run at their "+
			"class's trust level", r.Model, r.ApprovedBy)
	case StateRevoked:
		return fmt.Sprintf("G-M1 revoked for model %s: %s", r.Model, r.Reason)
	case StateModelOff:
		return fmt.Sprintf("G-M2: the review's model pass stopped calling %s: %s", r.Model,
			r.Reason)
	}
	return fmt.Sprintf("G-M2: the review's model pass calls %s again: %s", r.Model, r.Reason)
}
