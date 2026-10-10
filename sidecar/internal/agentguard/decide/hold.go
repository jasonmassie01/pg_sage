package decide

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// releaseTimeout bounds the rollback that ends a hold.
const releaseTimeout = 5 * time.Second

// HoldActive is the cross-database re-check (AGENTDB-SPEC §6.2.7).
// Principals live in the control database while writes commit in the
// target, so from the re-authorization until the target COMMIT returns,
// the executor holds the principal's row FOR SHARE on the control
// database. The kill takes FOR UPDATE on the same row with a 2 s
// lock_timeout, so it waits for in-flight commits and no new one starts
// once it holds the row. release ends the hold; it is idempotent.
//
// A principal that is not active is denied (agent_frozen, or
// agent_retired for a retired or unknown one).
func HoldActive(ctx context.Context, control *pgxpool.Pool, principalID string) (
	func(), error) {
	if principalID == "" {
		return nil, fmt.Errorf("%w: a hold needs a principal id", agentguard.ErrInvalid)
	}
	if control == nil {
		return nil, agentguard.ErrUnavailable
	}
	tx, err := control.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("agentguard: holding principal %s: %w", principalID, err)
	}
	release := releaser(tx)
	var status string
	err = tx.QueryRow(ctx, `/* pg_sage guard_principal_hold v1 */
SELECT status FROM sage.guard_principals WHERE id = $1 FOR SHARE`, principalID).
		Scan(&status)
	if err == nil && status == string(agentguard.StatusActive) {
		return release, nil
	}
	release()
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, &agentguard.DeniedError{Reason: agentguard.ReasonRetired,
			Detail: "principal " + principalID + " does not exist"}
	case err != nil:
		return nil, fmt.Errorf("agentguard: holding principal %s: %w", principalID, err)
	case status == string(agentguard.StatusFrozen):
		return nil, &agentguard.DeniedError{Reason: agentguard.ReasonFrozen,
			Detail: "principal " + principalID + " was frozen before its change committed"}
	}
	return nil, &agentguard.DeniedError{Reason: agentguard.ReasonRetired,
		Detail: "principal " + principalID + " is " + status}
}

func releaser(tx pgx.Tx) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
			defer cancel()
			_ = tx.Rollback(ctx) // read-only lock holder: nothing to keep
		})
	}
}
