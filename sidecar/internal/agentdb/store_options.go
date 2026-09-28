package agentdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MutationGate returns a non-nil error when provider or DDL mutations must
// not run (for example while the fleet emergency stop is engaged).
type MutationGate func(ctx context.Context) error

// StoreOptions carries runtime policy that the store enforces itself so the
// autonomous reconciler and the API apply identical rules.
type StoreOptions struct {
	// RequireBackupBeforeDestroy forces backup_required on every registered
	// deployment (agentdb.require_backup_before_destroy). Defaults to true.
	RequireBackupBeforeDestroy bool
	// LocalProvisioning allows local_postgres CREATE SCHEMA/DATABASE on the
	// store's own pool. Off by default: without a dedicated meta-database the
	// pool is a monitored production database.
	LocalProvisioning bool
	// Gate is consulted before every provider or DDL mutation. When nil the
	// store checks the persisted emergency_stop flag on its own pool.
	Gate MutationGate
}

// DefaultStoreOptions is the fail-closed default.
func DefaultStoreOptions() StoreOptions {
	return StoreOptions{RequireBackupBeforeDestroy: true}
}

func NewStoreWithOptions(pool *pgxpool.Pool, opts StoreOptions) *Store {
	return &Store{pool: pool, opts: opts}
}

// SetMutationGate installs an additional runtime gate (e.g. the in-memory
// fleet emergency stop). The persisted flag is always checked as well.
func (s *Store) SetMutationGate(gate MutationGate) {
	if s != nil {
		s.opts.Gate = gate
	}
}

// mutationAllowed fails closed: any error other than "flag never set" blocks.
func (s *Store) mutationAllowed(ctx context.Context) error {
	if s.opts.Gate != nil {
		if err := s.opts.Gate(ctx); err != nil {
			return fmt.Errorf("%w: %v", ErrEmergencyStop, err)
		}
	}
	var stopped bool
	err := s.pool.QueryRow(ctx, `/* pg_sage */
		SELECT COALESCE(bool_or(value='true'), false)
		FROM sage.config WHERE key='emergency_stop'`).Scan(&stopped)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "42P01":
		return nil // sage.config absent: no emergency stop was ever set here
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("%w: emergency stop state unreadable: %v", ErrEmergencyStop, err)
	case stopped:
		return ErrEmergencyStop
	}
	return nil
}
