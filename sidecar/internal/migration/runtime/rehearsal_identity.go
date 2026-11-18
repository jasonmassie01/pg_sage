package runtime

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	clonepkg "github.com/pg-sage/sidecar/internal/clone"
)

// A rehearsal runs DDL on a clone, so before the first step the runner
// proves the clone is not the production server (migration-07). The guard
// itself is clone.GuardedConnect (lab-04a), shared with the tuning
// rehearser; this file keeps only the migration runner's glue.

// ServerIdentity names one PostgreSQL server (clone.ServerIdentity).
type ServerIdentity = clonepkg.ServerIdentity

// IdentitySource reads the production server's identity when a
// rehearsal starts (clone.IdentitySource).
type IdentitySource = clonepkg.IdentitySource

// PoolIdentity is the identity of the server pool connects to: the
// monitored (production) database of a migration.
func PoolIdentity(pool *pgxpool.Pool) IdentitySource { return clonepkg.PoolIdentity(pool) }

// connectClone connects to the clone only after its DSN, then the server
// that answers, proved not to be production.
func (runner *PostgresRehearsalRunner) connectClone(
	ctx context.Context, dsn string,
) (*pgxpool.Pool, error) {
	return clonepkg.GuardedConnect(ctx, clonepkg.Clone{DSN: dsn}, runner.production)
}
