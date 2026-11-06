package gameday

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is a pool or connection the identity probes read through.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ClusterIdentity tells PostgreSQL clusters apart. The fault programs act
// on a whole cluster (ALTER SYSTEM, CHECKPOINT, replication slots, XID
// burn), so a game-day target is compared by cluster, never by DSN.
type ClusterIdentity struct {
	// SystemID is pg_control_system().system_identifier ("" when the
	// function is not permitted, as on some managed services). A
	// physical standby shares its primary's.
	SystemID string
	// PostmasterStart is pg_postmaster_start_time().
	PostmasterStart time.Time
	// CatalogHash is an md5 of oid:datname over the shared pg_database
	// catalog, equal in every database of one cluster.
	CatalogHash string
	InRecovery  bool
}

// The system identifier is read on its own so a revoked pg_control_system
// only weakens the identity.
const systemIDSQL = `/* pg_sage gameday_cluster_identity v1 */
SELECT system_identifier::text FROM pg_catalog.pg_control_system()`

const clusterIdentitySQL = `/* pg_sage gameday_cluster_identity v1 */
SELECT pg_catalog.pg_postmaster_start_time(),
       (SELECT pg_catalog.md5(pg_catalog.string_agg(d.oid::text || ':' || d.datname, ','
                ORDER BY d.oid))
          FROM pg_catalog.pg_database d),
       pg_catalog.pg_is_in_recovery()`

// ReadClusterIdentity reads the identity of the cluster behind q with
// read-only catalog probes; the caller bounds them with ctx. A
// pg_control_system privilege error leaves SystemID empty; anything else
// that fails is an error.
func ReadClusterIdentity(ctx context.Context, q Querier) (ClusterIdentity, error) {
	if q == nil {
		return ClusterIdentity{}, errors.New("read cluster identity: no connection")
	}
	var id ClusterIdentity
	err := q.QueryRow(ctx, systemIDSQL).Scan(&id.SystemID)
	if err != nil {
		var pgErr *pgconn.PgError
		if ctx.Err() != nil || !errors.As(err, &pgErr) {
			return ClusterIdentity{}, fmt.Errorf("read the system identifier: %w",
				errors.Join(ctx.Err(), err))
		}
		// Not permitted here: the identity is weaker, never wrong.
	}
	err = q.QueryRow(ctx, clusterIdentitySQL).Scan(&id.PostmasterStart, &id.CatalogHash,
		&id.InRecovery)
	if err != nil {
		return ClusterIdentity{}, fmt.Errorf("read the postmaster start time and catalog: %w",
			errors.Join(ctx.Err(), err))
	}
	if id.CatalogHash == "" || id.PostmasterStart.IsZero() {
		return ClusterIdentity{}, errors.New("read cluster identity: empty catalog hash " +
			"or start time")
	}
	return id, nil
}

// sameCluster: equal system identifiers when both are known, else an
// equal shared catalog and postmaster start time.
func sameCluster(a, b ClusterIdentity) bool {
	if a.SystemID != "" && b.SystemID != "" {
		return a.SystemID == b.SystemID
	}
	return a.CatalogHash == b.CatalogHash && a.PostmasterStart.Equal(b.PostmasterStart)
}
