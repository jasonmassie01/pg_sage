package store

import (
	"context"
	"fmt"
	"time"
)

// Enrollment by policy set (fleetpol-01b): the set-scoped claim, the
// enrollment milestones and the per-set numbers behind
// pg_sage_fleet_enroll_seconds and pg_sage_fleet_baseline_24h_ratio.

// Milestone is a recorded point of a discovered member's enrollment.
type Milestone string

// Milestones, in the order a member reaches them.
const (
	MilestoneEnrolled       Milestone = "enrolled"
	MilestoneGradeRequested Milestone = "grade_requested"
	MilestoneFirstGraded    Milestone = "first_graded"
	MilestoneBaseline       Milestone = "baseline"
)

// milestoneSQL records each milestone once: a later write keeps the first.
var milestoneSQL = map[Milestone]string{
	MilestoneEnrolled: `/* pg_sage */ UPDATE sage.discovered_databases
		SET enrolled_at = COALESCE(enrolled_at, $2) WHERE id = $1`,
	MilestoneGradeRequested: `/* pg_sage */ UPDATE sage.discovered_databases
		SET grade_requested_at = COALESCE(grade_requested_at, $2) WHERE id = $1`,
	MilestoneFirstGraded: `/* pg_sage */ UPDATE sage.discovered_databases
		SET first_graded_at = COALESCE(first_graded_at, $2) WHERE id = $1`,
	MilestoneBaseline: `/* pg_sage */ UPDATE sage.discovered_databases
		SET baseline_at = COALESCE(baseline_at, $2) WHERE id = $1`,
}

// EnrollStat is one policy set's enrollment numbers over the last 30 days:
// seconds from creation (the provider's time, else first seen) to
// enrolled at p50 and p95, and of the members whose first 24 hours are
// over (or that reached the baseline sooner), how many reached it in 24 h.
type EnrollStat struct {
	Set                    string
	Enrolled               int
	P50Seconds, P95Seconds float64
	Eligible, AtBaseline   int
}

// ClaimSet is Claim kept to the candidates of policy set set, enrolling at
// tier: maxLive counts the live discovered members of that tier only
// (fleetpol-01d), and a member still enrolling counts against both.
func (s *DiscoveryStore) ClaimSet(ctx context.Context, source, set, tier string, limit,
	maxLive int, now time.Time) ([]DiscoveredRow, error) {
	if !ValidTier(tier) {
		return nil, fmt.Errorf("%w: claim tier %q must be %s or %s", ErrValidation, tier,
			TierFull, TierObserveOnly)
	}
	return s.claim(ctx, source, &set, &tier, limit, maxLive, now)
}

// liveDiscoveredSQL counts the live discovered members ($2: of one tier,
// or all when NULL). An enrolling row has no member yet: it counts.
const liveDiscoveredSQL = `/* pg_sage */ SELECT count(*)
	 FROM sage.discovered_databases d
	 WHERE (d.status IN ('enrolled', 'adopted')
	        OR (d.status = 'enrolling' AND d.status_changed_at >= $1))
	   AND ($2::text IS NULL OR d.database_id IS NULL
	        OR COALESCE((SELECT m.tier FROM sage.databases m WHERE m.id = d.database_id),
	                    'full') = $2)`

// PendingGrades is every enrolled or adopted member with a set that has
// not reached the set's baseline yet: at most every member sage.databases
// holds (50 full and 2,000 observe-only, fleetpol-01d), so members that
// never meet their set's grades never starve the rest.
func (s *DiscoveryStore) PendingGrades(ctx context.Context) ([]DiscoveredRow, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT `+discoveredColumns+`
		 FROM sage.discovered_databases d
		 WHERE d.status IN ('enrolled', 'adopted') AND d.set_name IS NOT NULL
		   AND d.baseline_at IS NULL AND d.database_id IS NOT NULL
		 ORDER BY d.id LIMIT $1`, MaxFullMembers+MaxObserveOnlyMembers)
	if err != nil {
		return nil, fmt.Errorf("listing discovered members awaiting a grade: %w", err)
	}
	return scanDiscovered(rows)
}

// MarkMilestone records milestone m of row rowID at at, unless recorded.
func (s *DiscoveryStore) MarkMilestone(ctx context.Context, rowID int64, m Milestone,
	at time.Time) error {
	sql, ok := milestoneSQL[m]
	if !ok {
		return fmt.Errorf("%w: unknown enrollment milestone %q", ErrValidation, m)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(qctx, sql, rowID, at)
	if err != nil {
		return fmt.Errorf("recording %s of discovered database %d: %w", m, rowID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: discovered database %d", ErrNotFound, rowID)
	}
	return nil
}

const enrollStatsSQL = `/* pg_sage */ SELECT set_name, count(*),
	percentile_cont(0.5) WITHIN GROUP (ORDER BY took),
	percentile_cont(0.95) WITHIN GROUP (ORDER BY took),
	count(*) FILTER (WHERE reached OR enrolled_at <= $1::timestamptz - interval '24 hours'),
	count(*) FILTER (WHERE reached)
 FROM (SELECT set_name, enrolled_at,
	          GREATEST(0, extract(epoch FROM enrolled_at
	                   - COALESCE(created_at, first_seen)))::float8 AS took,
	          COALESCE(baseline_at <= enrolled_at + interval '24 hours', false) AS reached
	     FROM sage.discovered_databases
	    WHERE set_name IS NOT NULL AND enrolled_at IS NOT NULL
	      AND enrolled_at >= $1::timestamptz - interval '30 days') e
 GROUP BY set_name ORDER BY set_name`

// EnrollStats is each policy set's enrollment numbers as of now.
func (s *DiscoveryStore) EnrollStats(ctx context.Context, now time.Time) (
	[]EnrollStat, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, enrollStatsSQL, now)
	if err != nil {
		return nil, fmt.Errorf("reading enrollment numbers per policy set: %w", err)
	}
	defer rows.Close()
	var out []EnrollStat
	for rows.Next() {
		var st EnrollStat
		if err := rows.Scan(&st.Set, &st.Enrolled, &st.P50Seconds, &st.P95Seconds,
			&st.Eligible, &st.AtBaseline); err != nil {
			return nil, fmt.Errorf("scanning enrollment numbers: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading enrollment numbers per policy set: %w", err)
	}
	return out, nil
}

// candidateTags is c's tags for the jsonb column: never NULL.
func candidateTags(tags map[string]string) map[string]string {
	if tags == nil {
		return map[string]string{}
	}
	return tags
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
