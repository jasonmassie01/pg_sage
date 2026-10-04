package executor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// budgetLockNamespace is the first key of the budget advisory lock ("Sage"
// in ASCII); the second is the database id. Every sidecar authorizing for
// one database takes the same lock.
const budgetLockNamespace int32 = 0x53616765

// budgetTxKey carries the budget transaction from serializeBudget to the
// usage read and the decision record.
type budgetTxKey struct{}

// budgetLockKeys is the stable advisory lock key of this executor's
// database (0 outside meta-db mode, where the pool is the database).
func (e *Executor) budgetLockKeys() (int32, int32) {
	e.policyMu.RLock()
	defer e.policyMu.RUnlock()
	if e.databaseID == nil {
		return budgetLockNamespace, 0
	}
	return budgetLockNamespace, int32(*e.databaseID)
}

// serializeBudget opens the transaction the gate reads usage and records a
// budget-spending decision in, holding a transaction-scoped advisory lock
// (owner decision 2026-10-03), so two sidecars on one database cannot both
// take the last slot of a budget. done(commit) ends the transaction and
// with it the lock.
func (e *Executor) serializeBudget(
	ctx context.Context, _ policy.ActionRequest,
) (context.Context, func(bool) error, error) {
	if e.pool == nil {
		return ctx, nil, fmt.Errorf("the budget lock requires a database pool")
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return ctx, nil, fmt.Errorf("begin the budget transaction: %w", err)
	}
	namespace, database := e.budgetLockKeys()
	if _, err := tx.Exec(ctx, `/* pg_sage */ SELECT pg_advisory_xact_lock($1, $2)`,
		namespace, database); err != nil {
		if rollbackErr := tx.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
			e.logFn("executor", "roll back the budget transaction: %v", rollbackErr)
		}
		return ctx, nil, fmt.Errorf("take the budget lock: %w", err)
	}
	done := func(commit bool) error {
		if commit {
			return tx.Commit(ctx)
		}
		return tx.Rollback(context.WithoutCancel(ctx))
	}
	return context.WithValue(ctx, budgetTxKey{}, tx), done, nil
}

// budgetTx is the budget transaction ctx runs in, if any.
func budgetTx(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(budgetTxKey{}).(pgx.Tx)
	return tx, ok
}

// usageRow runs the usage read in the budget transaction when there is one.
func (e *Executor) usageRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx, ok := budgetTx(ctx); ok {
		return tx.QueryRow(ctx, sql, args...)
	}
	return e.pool.QueryRow(ctx, sql, args...)
}

// recordStandingDecision records a standing-gate decision in the ledger,
// inside the budget transaction when the gate holds the budget lock.
func (e *Executor) recordStandingDecision(
	ctx context.Context, policyVersion int64, request policy.ActionRequest,
	decision policy.Decision,
) (string, int64, error) {
	repository := ledger.NewPostgresRepository(e.pool)
	if tx, ok := budgetTx(ctx); ok {
		repository = ledger.NewTxRepository(tx)
	}
	input := ledgerInput(e.databaseID, policyVersion, request, decision)
	recorded, err := ledger.NewService(repository).RecordDecision(ctx, input)
	return recorded.EvidenceID, recorded.ID, err
}
