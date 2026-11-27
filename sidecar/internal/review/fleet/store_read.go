package fleet

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ScheduledIn is the members of the scope whose deep review was
// scheduled in period.
func (s *Store) ScheduledIn(ctx context.Context, period string) ([]string, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage review fleet */
		SELECT DISTINCT m.database_name FROM sage.review_fleet_member m
		  JOIN sage.review_fleet_pass p ON p.id = m.pass_id
		 WHERE p.fleet_scope = $1 AND m.scheduled_period = $2
		   AND m.state IN ('deep_scheduled', 'reviewed')
		 ORDER BY 1`, s.scope, period)
	if err != nil {
		return nil, fmt.Errorf("read the deep reviews of %s: %w", period, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the deep reviews of %s: %w", period, err)
	}
	return out, nil
}

// Carried is every member whose newest row waits: triaged and deferred to
// period or before. A failed pass's row (a pass left running is failed
// when the next starts) counts only once the member was scheduled in it;
// otherwise it would hide the earlier pass's wait, and the member would
// lose its place.
func (s *Store) Carried(ctx context.Context, period string) ([]Outlier, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage review fleet */
		SELECT pass_id, database_name, coalesce(rollup->>'id', ''), coalesce(score, 1),
		       reason, cites, source
		  FROM (SELECT DISTINCT ON (m.database_name) m.*
		          FROM sage.review_fleet_member m
		          JOIN sage.review_fleet_pass p ON p.id = m.pass_id
		         WHERE p.fleet_scope = $1
		           AND (p.status <> 'failed' OR m.state IN ('deep_scheduled', 'reviewed'))
		         ORDER BY m.database_name, m.pass_id DESC) newest
		 WHERE state = 'triaged' AND deferred_to IS NOT NULL AND deferred_to <= $2
		 ORDER BY database_name`, s.scope, period)
	if err != nil {
		return nil, fmt.Errorf("read the members waiting for %s: %w", period, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Outlier, error) {
		o := Outlier{Carried: true}
		err := row.Scan(&o.PassID, &o.Database, &o.ID, &o.Score, &o.Reason, &o.Cites,
			&o.Source)
		return o, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the members waiting for %s: %w", period, err)
	}
	return out, nil
}

// AwaitingReview is every scheduled deep review of the scope not yet run.
func (s *Store) AwaitingReview(ctx context.Context) ([]Scheduled, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage review fleet */
		SELECT m.pass_id, m.database_name, coalesce(m.deep_tokens, 0),
		       coalesce(m.score, 0), m.reason, coalesce(m.scheduled_period, ''),
		       coalesce(m.trigger_id, 0)
		  FROM sage.review_fleet_member m
		  JOIN sage.review_fleet_pass p ON p.id = m.pass_id
		 WHERE p.fleet_scope = $1 AND m.state = 'deep_scheduled'
		 ORDER BY m.pass_id, m.database_name`, s.scope)
	if err != nil {
		return nil, fmt.Errorf("read the scheduled deep reviews: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Scheduled, error) {
		var d Scheduled
		err := row.Scan(&d.PassID, &d.Database, &d.Tokens, &d.Score, &d.Reason, &d.Period,
			&d.Ref)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the scheduled deep reviews: %w", err)
	}
	return out, nil
}

// TierSpend is one review tier's spend in sage.llm_usage; CostUSD is nil
// when none of its rows is priced.
type TierSpend struct {
	Calls            int64    `json:"calls"`
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
}

// Spend is the usage ledger's spend of the three review tiers (purposes
// review_triage, review_fleet and review) over the days [from, to). The
// ledger is read where it is kept (the control database in fleet mode).
func (s *Store) Spend(ctx context.Context, from, to time.Time) (map[string]TierSpend, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage review fleet */
		SELECT purpose, sum(calls)::int8, sum(prompt_tokens)::int8,
		       sum(completion_tokens)::int8, sum(est_cost_usd)::float8
		  FROM sage.llm_usage
		 WHERE purpose = ANY($1) AND day >= $2::date AND day < $3::date
		 GROUP BY purpose`, []string{PurposeTriage, PurposeFleet, PurposeDeep},
		from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("read the review tiers' spend from sage.llm_usage: %w", err)
	}
	out := map[string]TierSpend{}
	defer rows.Close()
	for rows.Next() {
		var purpose string
		var t TierSpend
		if err := rows.Scan(&purpose, &t.Calls, &t.PromptTokens, &t.CompletionTokens,
			&t.CostUSD); err != nil {
			return nil, fmt.Errorf("scan the review tiers' spend: %w", err)
		}
		out[purpose] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the review tiers' spend from sage.llm_usage: %w", err)
	}
	return out, nil
}
