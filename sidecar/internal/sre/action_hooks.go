package sre

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Hooks for the Sage SRE action service (package sre/action, M5). The
// action service keeps its own table but writes to the investigation's
// event chain, so it needs the investigation row lock and the chain's
// append in its transaction. Nothing here executes anything.

// Store is the service's coordination store.
func (s *Service) Store() *PostgresStore { return s.store }

// Scope is the bound database identity, binding now if needed.
func (s *Service) Scope(ctx context.Context) (Scope, error) { return s.scope(ctx) }

// Pool is the coordination database.
func (s *PostgresStore) Pool() *pgxpool.Pool { return s.pool }

// Emit appends one event to the locked investigation's chain.
type Emit func(eventType string, payload map[string]any) error

// WithInvestigationLock runs fn in one transaction that holds the
// investigation's row lock, with an emitter for its event chain. Errors
// from beginning, locking, emitting and committing are classified like
// every store error (an outage is ErrMetadataUnavailable); fn's own
// errors are returned unchanged.
func (s *PostgresStore) WithInvestigationLock(ctx context.Context, scope Scope, id UUID,
	actor string, fn func(tx pgx.Tx, emit Emit) error) error {
	if err := validateIDs(scope, id); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return storeErr(ctx, "action transaction", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM sage.sre_investigations
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3 FOR UPDATE`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id)).Scan(&one)
	if err != nil {
		return storeErr(ctx, "lock investigation", noRows(err))
	}
	var emitErr error
	emit := func(typ string, payload map[string]any) error {
		if err := appendEvent(ctx, tx, scope, id, typ, actor, payload); err != nil {
			emitErr = storeErr(ctx, "append "+typ+" event", err)
			return emitErr
		}
		return nil
	}
	if err := fn(tx, emit); err != nil {
		if emitErr != nil {
			return emitErr
		}
		return err
	}
	return storeErr(ctx, "commit action", tx.Commit(ctx))
}

func noRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: investigation", ErrNotFound)
	}
	return err
}

// StoreError classifies a database error of the action store like every
// store error: a connection-level failure is ErrMetadataUnavailable.
func StoreError(ctx context.Context, op string, err error) error {
	return storeErr(ctx, op, err)
}

// RedactInto redacts a value through its JSON form into out (CHECK-30).
func RedactInto(in, out any) error { return redactInto(in, out) }

// LatestRevision keeps only the latest diagnosis revision of hypotheses.
func LatestRevision(hs []HypothesisRecord) []HypothesisRecord {
	latest, _ := latestRevision(hs)
	return latest
}
