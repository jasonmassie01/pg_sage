package changereview

import (
	"context"
	"math"
	"sort"
	"time"
)

// summaryOverrides reads, per rule, the decided reviews carrying its
// finding, how many a DBA overrode, and their reasons.
func (st *Store) summaryOverrides(ctx context.Context, s *Summary) error {
	rows, err := st.pool.Query(ctx, `/* pg_sage change_review_summary_rules v1 */
		SELECT rule, count(*), count(*) FILTER (WHERE dba_verdict = 'override'),
		  COALESCE((array_agg(DISTINCT dba_reason) FILTER (WHERE dba_verdict = 'override'
		    AND dba_reason IS NOT NULL))[1:$2], '{}')
		FROM (SELECT DISTINCT r.id, f->>'rule_id' AS rule, r.dba_verdict, r.dba_reason
		      FROM sage.change_review r, jsonb_array_elements(r.findings) f
		      WHERE r.created_at >= $1 AND r.state = 'complete'
		        AND r.dba_verdict IS NOT NULL AND COALESCE(f->>'rule_id', '') <> '') d
		GROUP BY rule`, s.Since, maxOverrideReasons)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rule string
		var o RuleOverrides
		if err := rows.Scan(&rule, &o.Reviews, &o.Overrides, &o.Reasons); err != nil {
			return err
		}
		s.OverrideByRule[rule] = o
	}
	return rows.Err()
}

// summaryFirstComments reads, per PR, the time from opening to pg_sage's
// first completed review.
func (st *Store) summaryFirstComments(ctx context.Context, s *Summary) error {
	return st.pool.QueryRow(ctx, `/* pg_sage change_review_summary_first v1 */
		SELECT COALESCE(array_agg(wait), '{}') FROM (
		  SELECT extract(epoch FROM min(completed_at) - min(pr_opened_at))::float8 AS wait
		  FROM sage.change_review
		  WHERE created_at >= $1 AND state = 'complete' AND pr_url IS NOT NULL
		    AND pr_opened_at IS NOT NULL
		  GROUP BY pr_url) w`, s.Since).Scan(&s.firstComments)
}

// summaryAutoClear reads the shadow auto-clear evidence of the last 90
// days.
func (st *Store) summaryAutoClear(ctx context.Context, s *Summary) error {
	a := &s.AutoClear
	return st.pool.QueryRow(ctx, `/* pg_sage change_review_summary_shadow v1 */
		SELECT count(*), count(*) FILTER (WHERE shadow_score <> 'unscored'),
		  count(*) FILTER (WHERE shadow_score = 'correct'),
		  count(*) FILTER (WHERE receipt_verdict = 'regressed')
		FROM sage.change_review
		WHERE created_at >= $1 AND state = 'complete' AND shadow_auto_clear`,
		st.now().Add(-AutoClearWindow)).Scan(&a.Eligible, &a.Scored, &a.Correct,
		&a.Regressions90d)
}

// finishSummary derives the rates, percentiles and readiness.
func finishSummary(s *Summary, now time.Time) {
	days := now.Sub(s.Since).Hours() / 24
	if days < 1 {
		days = 1
	}
	s.ReviewsPerWeek = round2(float64(s.Reviews) * 7 / days)
	if s.Reviews > 0 {
		s.TokensPerReview = round2(float64(s.tokens) / float64(s.Reviews))
	}
	for k, c := range s.Concurrence {
		c.Rate = ratio(c.N, c.Of)
		s.Concurrence[k] = c
	}
	for k, o := range s.OverrideByRule {
		o.Rate = ratio(o.Overrides, o.Reviews)
		if o.Reasons == nil {
			o.Reasons = []string{}
		}
		s.OverrideByRule[k] = o
	}
	s.DBAWaitP50Seconds = percentile(s.dbaWaits, 0.5)
	s.DBAWaitP90Seconds = percentile(s.dbaWaits, 0.9)
	s.FirstCommentP50Seconds = percentile(s.firstComments, 0.5)
	a := &s.AutoClear
	a.CorrectRate = ratio(a.Correct, a.Scored)
	a.Ready = a.Scored >= a.MinScored && a.CorrectRate != nil &&
		*a.CorrectRate >= a.MinCorrectRate && a.Regressions90d <= a.MaxRegressions
	s.Partial = len(s.Unavailable) > 0
}

func ratio(n, of int) *float64 {
	if of == 0 {
		return nil
	}
	r := round2(float64(n) / float64(of))
	return &r
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// percentile is the nearest-rank q-quantile of samples (nil when empty).
func percentile(samples []float64, q float64) *float64 {
	if len(samples) == 0 {
		return nil
	}
	s := append([]float64(nil), samples...)
	sort.Float64s(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	v := math.Round(s[i])
	return &v
}

// MergeSummaries merges per-database summaries into a fleet summary;
// unavailable names databases that could not be read (the result is then
// partial).
func MergeSummaries(parts []Summary, unavailable []string) Summary {
	since := time.Time{}
	if len(parts) > 0 {
		since = parts[0].Since
	}
	m := emptySummary(since)
	m.Unavailable = append(m.Unavailable, unavailable...)
	weeks := 0.0
	for _, p := range parts {
		m.Databases = append(m.Databases, p.Databases...)
		m.Reviews += p.Reviews
		m.FalseSafe += p.FalseSafe
		m.CaughtBeforeMerge += p.CaughtBeforeMerge
		m.HoursCredited += p.HoursCredited
		m.HoursPending += p.HoursPending
		m.tokens += int64(math.Round(p.TokensPerReview * float64(p.Reviews)))
		weeks += p.ReviewsPerWeek
		mergeCounts(m.VerdictMix, p.VerdictMix)
		mergeCounts(m.DBAVerdictMix, p.DBAVerdictMix)
		mergeCounts(m.Receipts, p.Receipts)
		mergeParts(&m, p)
	}
	finishSummary(&m, time.Now())
	m.ReviewsPerWeek = round2(weeks)
	m.HoursCredited, m.HoursPending = round2(m.HoursCredited), round2(m.HoursPending)
	return m
}

func mergeCounts(dst, src map[string]int) {
	for k, v := range src {
		dst[k] += v
	}
}

func mergeParts(m *Summary, p Summary) {
	for k, c := range p.Concurrence {
		cur := m.Concurrence[k]
		cur.N, cur.Of = cur.N+c.N, cur.Of+c.Of
		m.Concurrence[k] = cur
	}
	for k, o := range p.OverrideByRule {
		cur := m.OverrideByRule[k]
		cur.Reviews, cur.Overrides = cur.Reviews+o.Reviews, cur.Overrides+o.Overrides
		for _, r := range o.Reasons {
			if len(cur.Reasons) < maxOverrideReasons {
				cur.Reasons = append(cur.Reasons, r)
			}
		}
		m.OverrideByRule[k] = cur
	}
	m.dbaWaits = append(m.dbaWaits, p.dbaWaits...)
	m.firstComments = append(m.firstComments, p.firstComments...)
	a, b := &m.AutoClear, p.AutoClear
	a.Eligible, a.Scored = a.Eligible+b.Eligible, a.Scored+b.Scored
	a.Correct, a.Regressions90d = a.Correct+b.Correct, a.Regressions90d+b.Regressions90d
}
