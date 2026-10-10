package grants

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// registryPrivilegesSQL expands a principal's active grants into the
// privileges they give: each listed column of a relation grant (by
// attnum) and each schema USAGE.
const registryPrivilegesSQL = `/* pg_sage guard_grant v1 */
SELECT 'column', g.object_oid::int8, a.attnum, g.object_name || '.' || a.attname::text,
  p.priv
FROM sage.guard_grants g
JOIN pg_catalog.pg_attribute a ON a.attrelid = g.object_oid AND a.attname = ANY(g.columns)
  AND a.attnum > 0 AND NOT a.attisdropped
CROSS JOIN LATERAL unnest(g.privileges) AS p(priv)
WHERE g.principal_id = $1 AND g.object_kind = 'relation' AND g.state = 'active'
  AND g.revoked_at IS NULL
UNION
SELECT 'schema', g.object_oid::int8, 0::int2, g.object_name, p.priv
FROM sage.guard_grants g CROSS JOIN LATERAL unnest(g.privileges) AS p(priv)
WHERE g.principal_id = $1 AND g.object_kind = 'schema' AND g.state = 'active'
  AND g.revoked_at IS NULL`

// RegistryPrivileges is what a principal's active registry grants give,
// in core's Privilege shape, for agentguard.CheckEffectivePrivileges
// (G1-02: effective privileges = Guard grants + the PUBLIC baseline).
func RegistryPrivileges(ctx context.Context, q Querier,
	principalID string) ([]agentguard.Privilege, error) {
	if !agentguard.ValidID(principalID) {
		return nil, invalidf("principal %q", principalID)
	}
	rows, err := q.Query(ctx, registryPrivilegesSQL, principalID)
	if err != nil {
		return nil, fmt.Errorf("grants: reading registry privileges: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (agentguard.Privilege, error) {
		var p agentguard.Privilege
		var oid int64
		err := r.Scan(&p.Kind, &oid, &p.Attnum, &p.Name, &p.Privilege)
		p.OID = uint32(oid)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("grants: reading registry privileges: %w", err)
	}
	return out, nil
}
