package shadow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ClassSummary counts one class's shadow decisions by status and score;
// Counted are the scores the trust ledger counts as shadow evidence.
type ClassSummary struct {
	Family         string     `json:"family"`
	Class          string     `json:"class"`
	Total          int        `json:"total"`
	Pending        int        `json:"pending"`
	Correct        int        `json:"correct"`
	Incorrect      int        `json:"incorrect"`
	Neutral        int        `json:"neutral"`
	Unscored       int        `json:"unscored"`
	Counted        int        `json:"counted"`
	LastRecordedAt *time.Time `json:"last_recorded_at,omitempty"`
}

const summarySQL = `/* pg_sage */ SELECT family, action_class, count(*),
	count(*) FILTER (WHERE status = 'pending'),
	count(*) FILTER (WHERE score = 'correct'),
	count(*) FILTER (WHERE score = 'incorrect'),
	count(*) FILTER (WHERE score = 'neutral'),
	count(*) FILTER (WHERE score = 'unscored'),
	count(*) FILTER (WHERE counted), max(recorded_at)
	FROM sage.shadow_decision WHERE ($1 = '' OR action_class = $1)
	GROUP BY family, action_class ORDER BY family, action_class`

// Summary counts every class's decisions.
func (s *Store) Summary(ctx context.Context) ([]ClassSummary, error) {
	return s.summary(ctx, "")
}

func (s *Store) summary(ctx context.Context, class string) ([]ClassSummary, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.pool.Query(ctx, summarySQL, class)
	if err != nil {
		return nil, fmt.Errorf("shadow: summarize decisions: %w", err)
	}
	defer rows.Close()
	out := []ClassSummary{}
	for rows.Next() {
		var c ClassSummary
		if err := rows.Scan(&c.Family, &c.Class, &c.Total, &c.Pending, &c.Correct,
			&c.Incorrect, &c.Neutral, &c.Unscored, &c.Counted, &c.LastRecordedAt); err != nil {
			return nil, fmt.Errorf("shadow: scan summary: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shadow: summarize decisions: %w", err)
	}
	return out, nil
}

// History is a class's shadow record as an approval card shows it, and
// the shadow decision of the proposal itself, if pg_sage recorded one.
type History struct {
	Family       string       `json:"family"`
	Class        string       `json:"class"`
	Summary      ClassSummary `json:"summary"`
	ThisProposal *Decision    `json:"this_proposal,omitempty"`
}

// History reads class's record and the newest decision of the proposal:
// the same shape first, else the same finding.
func (s *Store) History(ctx context.Context, class string, findingID int64,
	shape string) (History, error) {
	if !classPattern.MatchString(class) {
		return History{}, fmt.Errorf("%w: class %q", ErrInvalid, class)
	}
	h := History{Family: classFamily(class), Class: class,
		Summary: ClassSummary{Family: classFamily(class), Class: class}}
	sums, err := s.summary(ctx, class)
	if err != nil {
		return History{}, err
	}
	if len(sums) > 0 {
		h.Summary = sums[0]
		h.Family = sums[0].Family
	}
	if h.Summary.Total == 0 {
		return h, nil
	}
	d, err := scanDecision(s.pool.QueryRow(ctx, `/* pg_sage */ SELECT `+decisionColumns+`
		FROM sage.shadow_decision
		WHERE action_class = $1 AND (shape = $3 OR ($2 > 0 AND finding_id = $2))
		ORDER BY (shape = $3) DESC, recorded_at DESC, id DESC LIMIT 1`,
		class, findingID, shape))
	if errors.Is(err, pgx.ErrNoRows) {
		return h, nil
	}
	if err != nil {
		return History{}, fmt.Errorf("shadow: read proposal's decision: %w", err)
	}
	h.ThisProposal = &d
	return h, nil
}
