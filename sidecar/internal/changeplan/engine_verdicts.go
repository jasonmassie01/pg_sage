package changeplan

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/verify"
)

// DBVerdicts reads a step's verdict from the ledger: the verifier's stored
// outcome (verify.OutcomeStore) and the action's own outcome.
type DBVerdicts struct {
	pool     *pgxpool.Pool
	outcomes *verify.OutcomeStore
}

// NewDBVerdicts reads verdicts from pool (the monitored database).
func NewDBVerdicts(pool *pgxpool.Pool) *DBVerdicts {
	return &DBVerdicts{pool: pool, outcomes: verify.NewOutcomeStore(pool)}
}

// Verdict implements VerdictSource. An action without an outcome row yet
// has a pending verdict.
func (d *DBVerdicts) Verdict(ctx context.Context, actionLogID int64) (ActionVerdict, error) {
	var v ActionVerdict
	err := d.pool.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(outcome, '')
		FROM sage.action_log WHERE id = $1`, actionLogID).Scan(&v.ActionOutcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return ActionVerdict{}, fmt.Errorf("changeplan: action %d: %w", actionLogID,
			ErrNotFound)
	}
	if err != nil {
		return ActionVerdict{}, fmt.Errorf("changeplan: read action %d: %w", actionLogID, err)
	}
	o, err := d.outcomes.Get(ctx, actionLogID)
	if errors.Is(err, verify.ErrOutcomeNotFound) {
		return v, nil
	}
	if err != nil {
		return ActionVerdict{}, fmt.Errorf("changeplan: read the verdict of action %d: %w",
			actionLogID, err)
	}
	v.Verdict, v.DecidedAt = o.Verdict, o.DecidedAt
	return v, nil
}
