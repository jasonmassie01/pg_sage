package changereview

import (
	"context"
	"fmt"
	"time"
)

// Shadow auto-clear readiness bar: the evidence a promotion would need.
// Shadow only: nothing is cleared automatically in this build.
const (
	AutoClearMinScored      = 30
	AutoClearMinCorrectRate = 0.97
	AutoClearMaxRegressions = 0
	AutoClearWindow         = 90 * 24 * time.Hour
	AutoClearLabel          = "shadow only — not enabled"
	maxOverrideReasons      = 5
)

// Rate is n of of, and their ratio (nil when of is 0).
type Rate struct {
	N    int      `json:"n"`
	Of   int      `json:"of"`
	Rate *float64 `json:"rate"`
}

// RuleOverrides is how often DBAs overrode decided reviews carrying one
// rule's finding, with up to five stated reasons.
type RuleOverrides struct {
	Reviews   int      `json:"reviews"`
	Overrides int      `json:"overrides"`
	Rate      *float64 `json:"rate"`
	Reasons   []string `json:"reasons"`
}

// AutoClearReadiness is the shadow auto-clear evidence over the last 90
// days against the bar (30 scored, 97% correct, no regressions).
type AutoClearReadiness struct {
	Eligible       int      `json:"eligible"`
	Scored         int      `json:"scored"`
	Correct        int      `json:"correct"`
	CorrectRate    *float64 `json:"correct_rate"`
	Regressions90d int      `json:"regressions_90d"`
	MinScored      int      `json:"min_scored"`
	MinCorrectRate float64  `json:"min_correct_rate"`
	MaxRegressions int      `json:"max_regressions"`
	Ready          bool     `json:"ready"`
	Label          string   `json:"label"`
}

// Summary is the Reviews-page and GET /api/v1/change-reviews/summary
// figures, measured from sage.change_review, for one database or merged
// across the fleet (Unavailable lists databases that could not be read,
// and Partial is then true).
type Summary struct {
	Databases               []string                 `json:"databases"`
	Unavailable             []string                 `json:"unavailable"`
	Partial                 bool                     `json:"partial"`
	Since                   time.Time                `json:"since"`
	Reviews                 int                      `json:"reviews"`
	ReviewsPerWeek          float64                  `json:"reviews_per_week"`
	VerdictMix              map[string]int           `json:"verdict_mix"`
	DBAVerdictMix           map[string]int           `json:"dba_verdict_mix"`
	DBAWaitP50Seconds       *float64                 `json:"dba_wait_p50_seconds"`
	DBAWaitP90Seconds       *float64                 `json:"dba_wait_p90_seconds"`
	FirstCommentP50Seconds  *float64                 `json:"first_comment_p50_seconds"`
	Concurrence             map[string]Rate          `json:"concurrence"`
	Receipts                map[string]int           `json:"receipts"`
	FalseSafe               int                      `json:"false_safe"`
	OverrideByRule          map[string]RuleOverrides `json:"override_by_rule"`
	CaughtBeforeMerge       int                      `json:"caught_before_merge"`
	HoursCredited           float64                  `json:"hours_credited"`
	HoursPending            float64                  `json:"hours_pending"`
	TokensPerReview         float64                  `json:"tokens_per_review"`
	AutoClear               AutoClearReadiness       `json:"auto_clear"`
	dbaWaits, firstComments []float64
	tokens                  int64
}

// Summary reads the figures of reviews created since (not in the future).
func (st *Store) Summary(ctx context.Context, since time.Time) (Summary, error) {
	now := st.now()
	if since.After(now) {
		return Summary{}, invalid("since %s is in the future", since.Format(time.RFC3339))
	}
	s := emptySummary(since)
	s.Databases = []string{st.database}
	steps := []func(context.Context, *Summary) error{st.summaryCounts, st.summaryMixes,
		st.summaryOverrides, st.summaryFirstComments, st.summaryAutoClear}
	for _, step := range steps {
		if err := step(ctx, &s); err != nil {
			return Summary{}, fmt.Errorf("%w: read the review summary: %v", ErrUnavailable,
				err)
		}
	}
	finishSummary(&s, now)
	return s, nil
}

func emptySummary(since time.Time) Summary {
	return Summary{Databases: []string{}, Unavailable: []string{}, Since: utc(since),
		VerdictMix: map[string]int{}, DBAVerdictMix: map[string]int{},
		Concurrence: map[string]Rate{}, Receipts: map[string]int{},
		OverrideByRule: map[string]RuleOverrides{},
		AutoClear: AutoClearReadiness{MinScored: AutoClearMinScored,
			MinCorrectRate: AutoClearMinCorrectRate, MaxRegressions: AutoClearMaxRegressions,
			Label: AutoClearLabel}}
}

// summaryCounts reads totals, credit hours, tokens, false-safe, caught
// changes and the DBA wait samples.
func (st *Store) summaryCounts(ctx context.Context, s *Summary) error {
	var credited, pending int64
	err := st.pool.QueryRow(ctx, `/* pg_sage change_review_summary v1 */
		SELECT count(*), COALESCE(sum(tokens), 0),
		  count(*) FILTER (WHERE verdict = 'safe' AND receipt_verdict = 'regressed'),
		  COALESCE(sum(credited_minutes) FILTER (WHERE credit_state = 'pending'), 0),
		  COALESCE(array_agg(extract(epoch FROM dba_decided_at - completed_at)::float8)
		    FILTER (WHERE dba_decided_at IS NOT NULL AND verdict_carried_from IS NULL),
		    '{}'),
		  count(*) FILTER (WHERE verdict = 'risky' AND dba_verdict = 'concur'
		    AND pr_url IS NOT NULL AND EXISTS (SELECT 1 FROM sage.change_review later
		      WHERE later.pr_url = r.pr_url AND later.created_at > r.created_at
		        AND later.state = 'complete' AND later.verdict <> 'risky')),
		  (SELECT COALESCE(sum(credited_minutes), 0) FROM sage.change_review
		    WHERE credit_state = 'final' AND credit_final_at >= $1)
		FROM sage.change_review r WHERE created_at >= $1 AND state = 'complete'`, s.Since).
		Scan(&s.Reviews, &s.tokens, &s.FalseSafe, &pending, &s.dbaWaits,
			&s.CaughtBeforeMerge, &credited)
	s.HoursCredited, s.HoursPending = float64(credited)/60, float64(pending)/60
	return err
}

// summaryMixes reads the verdict, DBA verdict and receipt mixes and the
// concurrence per verdict class.
func (st *Store) summaryMixes(ctx context.Context, s *Summary) error {
	rows, err := st.pool.Query(ctx, `/* pg_sage change_review_summary_mix v1 */
		SELECT verdict, COALESCE(dba_verdict, ''), receipt_verdict, count(*)
		FROM sage.change_review WHERE created_at >= $1 AND state = 'complete'
		GROUP BY 1, 2, 3`, s.Since)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var verdict, dba, receipt string
		var n int
		if err := rows.Scan(&verdict, &dba, &receipt, &n); err != nil {
			return err
		}
		s.VerdictMix[verdict] += n
		s.Receipts[receipt] += n
		c := s.Concurrence[verdict]
		if dba != "" {
			s.DBAVerdictMix[dba] += n
			c.Of += n
		}
		if dba == DBAConcur {
			c.N += n
		}
		s.Concurrence[verdict] = c
	}
	return rows.Err()
}
