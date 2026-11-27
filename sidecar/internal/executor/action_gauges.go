package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ActionGauges are the action ledger values /metrics exports as gauges,
// read by the metrics refresher (never on the scrape path).
type ActionGauges struct {
	// PendingByRisk counts approvals a person can act on now: pending,
	// past any cooldown and not expired, by risk (safe, moderate, high,
	// other). The three known risks are always present.
	PendingByRisk map[string]int64
	// LastFailure is when the latest failed, partial or rollback_failed
	// outcome was recorded; zero when none was.
	LastFailure time.Time
}

// actionGaugeQuerier is a pool or a transaction.
type actionGaugeQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// knownRisks are the risk label values besides other.
var knownRisks = map[string]bool{"safe": true, "moderate": true, "high": true}

// ReadActionGauges reads the pending approvals and the last failure. Both
// reads are bounded by the caller's context; an error returns no values.
func ReadActionGauges(ctx context.Context, q actionGaugeQuerier) (ActionGauges, error) {
	pending, err := readPendingApprovals(ctx, q)
	if err != nil {
		return ActionGauges{}, err
	}
	last, err := readLastFailure(ctx, q)
	if err != nil {
		return ActionGauges{}, err
	}
	return ActionGauges{PendingByRisk: pending, LastFailure: last}, nil
}

func readPendingApprovals(ctx context.Context, q actionGaugeQuerier) (map[string]int64,
	error) {
	rows, err := q.Query(ctx, `/* pg_sage executor */ SELECT lower(action_risk), count(*)
		FROM sage.action_queue
		WHERE status = 'pending'
		  AND (cooldown_until IS NULL OR cooldown_until <= now())
		  AND (expires_at IS NULL OR expires_at > now())
		GROUP BY 1`)
	if err != nil {
		return nil, fmt.Errorf("count pending approvals: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{"safe": 0, "moderate": 0, "high": 0}
	for rows.Next() {
		var risk string
		var n int64
		if err := rows.Scan(&risk, &n); err != nil {
			return nil, fmt.Errorf("count pending approvals: %w", err)
		}
		risk = strings.TrimSpace(risk)
		if !knownRisks[risk] {
			risk = classOther
		}
		out[risk] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count pending approvals: %w", err)
	}
	return out, nil
}

func readLastFailure(ctx context.Context, q actionGaugeQuerier) (time.Time, error) {
	var last *time.Time
	err := q.QueryRow(ctx, `/* pg_sage executor */ SELECT max(COALESCE(measured_at, executed_at))
		FROM sage.action_log
		WHERE outcome IN ('failed', 'partial', 'rollback_failed')`).Scan(&last)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the last failed action: %w", err)
	}
	if last == nil {
		return time.Time{}, nil
	}
	return last.UTC(), nil
}
