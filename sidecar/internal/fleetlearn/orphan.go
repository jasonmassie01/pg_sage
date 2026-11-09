package fleetlearn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultOrphanScopeAge is how long after its lease expired a fleet scope
// counts as orphaned (fleet_learning.orphan_scope_days, 14).
const DefaultOrphanScopeAge = 14 * 24 * time.Hour

// pruneOrphanLeasesSQL drops the lease rows of other scopes that expired
// before the cutoff; this store's own scope is never touched.
const pruneOrphanLeasesSQL = `/* pg_sage */ DELETE FROM sage.fleet_leader_lease
	WHERE scope <> $1 AND expires_at < now() - $2::float8 * interval '1 second'
	RETURNING scope`

// PruneOrphanScopes deletes the fingerprints, outcome digests and lease
// rows of every other scope whose lease expired more than olderThan ago:
// a fleet whose YAML database names changed, or that was retired. It
// returns the scopes dropped. A scope without a lease row (election off)
// is kept: nothing says when it was last live.
func (s *Store) PruneOrphanScopes(ctx context.Context, f Fence,
	olderThan time.Duration) ([]string, error) {
	if olderThan <= 0 {
		return nil, fmt.Errorf("fleetlearn: prune orphan scopes: age must be positive, "+
			"got %v", olderThan)
	}
	var scopes []string
	err := s.inTx(ctx, "prune orphan scopes", f, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, pruneOrphanLeasesSQL, s.scope, olderThan.Seconds())
		if err != nil {
			return err
		}
		scopes, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(scopes) == 0 {
			return err
		}
		for _, table := range []string{"sage.fleet_fingerprint", "sage.fleet_outcome_digest"} {
			if _, err := tx.Exec(ctx, `/* pg_sage */ DELETE FROM `+table+
				` WHERE fleet_scope = ANY($1)`, scopes); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return scopes, nil
}

// pruneOrphanScopes runs PruneOrphanScopes on the leader's cycle. A lost
// lease stops the cycle; any other failure is logged and the cycle goes
// on, since orphaned rows only cost space.
func (s *Service) pruneOrphanScopes(ctx context.Context, f Fence) error {
	age := s.set.OrphanScopeAge
	if age <= 0 {
		age = DefaultOrphanScopeAge
	}
	scopes, err := s.store.PruneOrphanScopes(ctx, f, age)
	if errors.Is(err, ErrFenced) {
		return err
	}
	if err != nil {
		s.logFn("WARN", "fleet learning: orphaned fleet scopes not pruned: %v", err)
		return nil
	}
	if len(scopes) > 0 {
		s.logFn("INFO", "fleet learning: pruned %d orphaned fleet scopes (lease expired "+
			"more than %s ago)", len(scopes), age)
	}
	return nil
}
