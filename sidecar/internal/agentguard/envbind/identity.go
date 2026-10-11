package envbind

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const liveIdentitySQL = `/* pg_sage agent_env_identity v1 */
SELECT d.oid::bigint, pg_catalog.pg_is_in_recovery()
FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()`

// The system identifier needs EXECUTE on pg_control_system(), which managed
// services may revoke; it is read on its own so its absence only weakens
// the identity.
const systemIdentifierSQL = `/* pg_sage agent_env_identity v1 */
SELECT system_identifier::text FROM pg_catalog.pg_control_system()`

// ReadIdentity reads the live identity tuple of the database behind pool.
// providerRef is the provider resource id from cloud telemetry ("" when
// none). An unreadable system identifier leaves it empty; a connection
// failure is an error.
func ReadIdentity(ctx context.Context, pool *pgxpool.Pool, providerRef string) (Identity,
	error) {
	if pool == nil {
		return Identity{}, errors.New("envbind: read identity: no connection pool")
	}
	conn := pool.Config().ConnConfig
	id := Identity{ProviderRef: providerRef,
		Target: fmt.Sprintf("%s:%d/%s", conn.Host, conn.Port, conn.Database)}
	if providerRef != "" {
		id.RefSource = RefCloudTel
	}
	var oid int64
	if err := pool.QueryRow(ctx, liveIdentitySQL).Scan(&oid, &id.InRecovery); err != nil {
		return id, fmt.Errorf("envbind: read identity of %s: %w", id.Target, err)
	}
	id.DBOID = uint32(oid)
	err := pool.QueryRow(ctx, systemIdentifierSQL).Scan(&id.SystemIdentifier)
	var pgErr *pgconn.PgError
	if err != nil && ctx.Err() == nil && errors.As(err, &pgErr) {
		// Not permitted (or not available) here: the identity is weaker,
		// never wrong.
		id.SystemIdentifier = ""
		return id, nil
	}
	if err != nil {
		return id, fmt.Errorf("envbind: read system identifier of %s: %w", id.Target, err)
	}
	return id, nil
}
