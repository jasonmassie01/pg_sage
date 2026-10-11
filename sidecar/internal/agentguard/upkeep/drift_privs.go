package upkeep

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/grants"
)

// privileges compares role's privileges in db with its registry grants
// (the broker login's; the direct-lane role has none in G1) plus the PUBLIC
// baseline, corrects the excess pg_sage granted itself, and reports the
// rest and what is missing.
func (p *clusterPass) privileges(ctx context.Context, db agentguard.KillTarget,
	cr agentguard.ClusterRole, role string) (RoleDrift, error) {
	var d RoleDrift
	var reg []agentguard.Privilege
	if role == cr.BrokerRole {
		var err error
		if reg, err = grants.RegistryPrivileges(ctx, db.Pool, cr.PrincipalID); err != nil {
			return d, err
		}
	}
	rep, err := agentguard.CheckEffectivePrivileges(ctx, db.Pool, role, reg)
	if errors.Is(err, agentguard.ErrNoBaseline) {
		return d, fmt.Errorf("no PUBLIC baseline is recorded in %s, so its privileges "+
			"cannot be compared; run the preflight", db.Name)
	}
	if err != nil {
		return d, err
	}
	if d.Narrowing, err = missingPrivileges(ctx, db, role, reg); err != nil {
		return d, err
	}
	if len(rep.Excess) == 0 {
		return d, nil
	}
	own, err := classifyExcess(ctx, db, role, rep.Excess, &d)
	if err != nil {
		return d, err
	}
	return d, p.correct(ctx, db, cr, role, &d, own)
}

const missingSQL = `/* pg_sage agent_drift_missing v1 */
SELECT x.kind, x.name, x.priv
FROM unnest($2::text[], $3::oid[], $4::int2[], $5::text[], $6::text[])
  AS x(kind, oid, attnum, name, priv)
WHERE NOT COALESCE(CASE x.kind
  WHEN 'column' THEN pg_catalog.has_column_privilege($1, x.oid, x.attnum, x.priv)
  WHEN 'schema' THEN pg_catalog.has_schema_privilege($1, x.oid, x.priv)
  ELSE pg_catalog.has_table_privilege($1, x.oid, x.priv) END, true)`

// missingPrivileges lists the registry privileges role does not hold (an
// object dropped since is not missing: the check answers NULL).
func missingPrivileges(ctx context.Context, db agentguard.KillTarget, role string,
	reg []agentguard.Privilege) ([]string, error) {
	if len(reg) == 0 {
		return nil, nil
	}
	n := len(reg)
	kinds, oids, attnums, names, privs := make([]string, n), make([]uint32, n),
		make([]int16, n), make([]string, n), make([]string, n)
	for i, p := range reg {
		kinds[i], oids[i], attnums[i], names[i], privs[i] = p.Kind, p.OID, p.Attnum,
			p.Name, p.Privilege
	}
	rows, err := db.Pool.Query(ctx, missingSQL, role, kinds, oids, attnums, names, privs)
	if err != nil {
		return nil, fmt.Errorf("reading missing grants of %s in %s: %w", role, db.Name, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var kind, name, priv string
		err := r.Scan(&kind, &name, &priv)
		return fmt.Sprintf("granted %s on %s %s is missing", priv, kind, name), err
	})
}

// excessSQL finds, for each excess privilege, the ACL entry that gives it:
// to the role itself (from pg_sage's role or another grantor) or to PUBLIC,
// and the REVOKE that takes it away. No entry means it comes through a
// membership.
const excessSQL = `/* pg_sage agent_drift_excess v1 */
WITH me AS (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user),
r AS (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1)
SELECT x.kind, x.name, x.priv, COALESCE(a.grantee = 0, false),
  COALESCE(a.grantor = (SELECT oid FROM me), false),
  COALESCE(pg_catalog.pg_get_userbyid(a.grantor)::text, ''),
  COALESCE(pg_catalog.format('REVOKE %s %s FROM %s GRANTED BY %I', x.priv,
    CASE x.kind
      WHEN 'column' THEN pg_catalog.format('(%I) ON TABLE %s', (SELECT t.attname
        FROM pg_catalog.pg_attribute t WHERE t.attrelid = x.oid AND t.attnum = x.attnum),
        x.oid::pg_catalog.regclass)
      WHEN 'relation' THEN pg_catalog.format('ON %s %s', CASE WHEN (SELECT c.relkind
        FROM pg_catalog.pg_class c WHERE c.oid = x.oid) = 'S' THEN 'SEQUENCE'
        ELSE 'TABLE' END, x.oid::pg_catalog.regclass)
      WHEN 'schema' THEN pg_catalog.format('ON SCHEMA %I', x.name)
      WHEN 'function' THEN pg_catalog.format('ON FUNCTION %s',
        x.oid::pg_catalog.regprocedure)
      ELSE pg_catalog.format('ON DATABASE %I', x.name) END,
    CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.quote_ident($1) END,
    pg_catalog.pg_get_userbyid(a.grantor)), '')
FROM unnest($2::text[], $3::oid[], $4::int2[], $5::text[], $6::text[])
  AS x(kind, oid, attnum, name, priv)
LEFT JOIN LATERAL (
  SELECT e.grantee, e.grantor FROM pg_catalog.aclexplode(CASE x.kind
    WHEN 'column' THEN (SELECT t.attacl FROM pg_catalog.pg_attribute t
      WHERE t.attrelid = x.oid AND t.attnum = x.attnum)
    WHEN 'relation' THEN (SELECT c.relacl FROM pg_catalog.pg_class c WHERE c.oid = x.oid)
    WHEN 'schema' THEN (SELECT n.nspacl FROM pg_catalog.pg_namespace n WHERE n.oid = x.oid)
    WHEN 'function' THEN (SELECT f.proacl FROM pg_catalog.pg_proc f WHERE f.oid = x.oid)
    ELSE (SELECT b.datacl FROM pg_catalog.pg_database b WHERE b.oid = x.oid) END) e
  WHERE e.privilege_type = x.priv AND e.grantee IN ((SELECT oid FROM r), 0)
  ORDER BY e.grantee = 0, e.grantor = (SELECT oid FROM me) DESC LIMIT 1) a ON true`

// classifyExcess reports every excess privilege as widening drift, adds a
// Fix for those pg_sage does not own, and returns the REVOKEs it owns.
func classifyExcess(ctx context.Context, db agentguard.KillTarget, role string,
	excess []agentguard.Privilege, d *RoleDrift) ([]string, error) {
	n := len(excess)
	kinds, oids, attnums, names, privs := make([]string, n), make([]uint32, n),
		make([]int16, n), make([]string, n), make([]string, n)
	for i, p := range excess {
		kinds[i], oids[i], attnums[i], names[i], privs[i] = p.Kind, p.OID, p.Attnum,
			p.Name, p.Privilege
	}
	rows, err := db.Pool.Query(ctx, excessSQL, role, kinds, oids, attnums, names, privs)
	if err != nil {
		return nil, fmt.Errorf("reading the grantors of %s's excess privileges in %s: %w",
			role, db.Name, err)
	}
	defer rows.Close()
	var own, fix []string
	for rows.Next() {
		var kind, name, priv, grantor, stmt string
		var public, mine bool
		if err := rows.Scan(&kind, &name, &priv, &public, &mine, &grantor,
			&stmt); err != nil {
			return nil, fmt.Errorf("reading excess privileges in %s: %w", db.Name, err)
		}
		d.Widening = append(d.Widening, fmt.Sprintf("has %s on %s %s, not granted by "+
			"governance", priv, kind, name))
		switch {
		case stmt == "":
			fix = append(fix, fmt.Sprintf("-- %s on %s %s comes through a membership "+
				"of %s; revoke that membership", priv, kind, name, role))
		case public:
			fix = append(fix, "-- granted to PUBLIC after the preflight: "+stmt+
				"; (or re-run the preflight to accept it)")
		case mine:
			own = append(own, stmt)
		default:
			fix = append(fix, "-- as "+grantor+": "+stmt+";")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading excess privileges in %s: %w", db.Name, err)
	}
	d.Fix = strings.Join(fix, "\n")
	return own, nil
}
