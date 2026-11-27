package shadow

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/value"
)

// ClassSummary counts one class's shadow decisions by status and score;
// Counted are the scores the trust ledger counts as shadow evidence.
// PotentialMinutes is the current toil model's base minutes over the
// decisions that are counted and correct: what the class would have
// saved, never credited. It is nil when the class has no toil model.
type ClassSummary struct {
	Family           string     `json:"family"`
	Class            string     `json:"class"`
	Total            int        `json:"total"`
	Pending          int        `json:"pending"`
	Correct          int        `json:"correct"`
	Incorrect        int        `json:"incorrect"`
	Neutral          int        `json:"neutral"`
	Unscored         int        `json:"unscored"`
	Counted          int        `json:"counted"`
	PotentialMinutes *int       `json:"potential_minutes"`
	LastRecordedAt   *time.Time `json:"last_recorded_at,omitempty"`

	countedCorrect int
}

const summarySQL = `/* pg_sage */ SELECT family, action_class, count(*),
	count(*) FILTER (WHERE status = 'pending'),
	count(*) FILTER (WHERE score = 'correct'),
	count(*) FILTER (WHERE score = 'incorrect'),
	count(*) FILTER (WHERE score = 'neutral'),
	count(*) FILTER (WHERE score = 'unscored'),
	count(*) FILTER (WHERE counted),
	count(*) FILTER (WHERE counted AND score = 'correct'), max(recorded_at)
	FROM sage.shadow_decision WHERE ($1 = '' OR action_class = $1)
	GROUP BY family, action_class ORDER BY family, action_class`

// Summary counts every class's decisions.
func (s *Store) Summary(ctx context.Context) ([]ClassSummary, error) {
	return s.summary(ctx, "")
}

// SummaryFor counts the decisions of class ("" for every class); a class
// nobody recorded is an empty summary, a malformed one ErrInvalid.
func (s *Store) SummaryFor(ctx context.Context, class string) ([]ClassSummary, error) {
	if class != "" && !classPattern.MatchString(class) {
		return nil, fmt.Errorf("%w: class %q", ErrInvalid, class)
	}
	return s.summary(ctx, class)
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
			&c.Incorrect, &c.Neutral, &c.Unscored, &c.Counted, &c.countedCorrect,
			&c.LastRecordedAt); err != nil {
			return nil, fmt.Errorf("shadow: scan summary: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shadow: summarize decisions: %w", err)
	}
	if err := s.pricePotential(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// pricePotential sets each modelled class's potential minutes from the
// current toil model of its class (value.ToilActionTypeForClass).
func (s *Store) pricePotential(ctx context.Context, sums []ClassSummary) error {
	types := []string{}
	for _, c := range sums {
		if t := value.ToilActionTypeForClass(c.Class); t != "" {
			types = append(types, t)
		}
	}
	if len(types) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT action_type, base_minutes::float8
		FROM sage.toil_model WHERE effective_to IS NULL AND action_type = ANY($1)`, types)
	if err != nil {
		return fmt.Errorf("shadow: read toil models: %w", err)
	}
	minutes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (toilRow, error) {
		var t toilRow
		return t, r.Scan(&t.actionType, &t.minutes)
	})
	if err != nil {
		return fmt.Errorf("shadow: read toil models: %w", err)
	}
	base := make(map[string]float64, len(minutes))
	for _, t := range minutes {
		base[t.actionType] = t.minutes
	}
	for i := range sums {
		if b, ok := base[value.ToilActionTypeForClass(sums[i].Class)]; ok {
			n := int(math.Round(b * float64(sums[i].countedCorrect)))
			sums[i].PotentialMinutes = &n
		}
	}
	return nil
}

// toilRow is one active toil model.
type toilRow struct {
	actionType string
	minutes    float64
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
