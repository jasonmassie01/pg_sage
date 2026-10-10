package agentguard

import (
	"context"
	"fmt"
)

// Residue is a privilege an agent role holds from a grantor other than
// pg_sage's role (§6.6: revoke_incomplete). pg_sage cannot revoke it; the
// grantor must.
type Residue struct {
	Role    string `json:"role"`
	Object  string `json:"object"`
	Grantor string `json:"grantor"`
}

func (r Residue) String() string {
	return fmt.Sprintf("%s on %s granted by %s", r.Role, r.Object, r.Grantor)
}

// residueSQL starts from the role's ACL dependencies in this database and
// the shared catalogs (pg_shdepend, indexed by role), so it never scans
// the catalog, and keeps the entries whose grantor is not current_user.
// Object kinds Guard never grants (types, languages, …) are not inspected
// here; DROP ROLE still refuses them.
const residueSQL = `/* pg_sage guard_role_residue v1 */
SELECT r.rolname::text, pg_catalog.pg_describe_object(d.classid, d.objid, d.objsubid),
  pg_catalog.pg_get_userbyid(a.grantor)::text
FROM pg_catalog.pg_shdepend d
JOIN pg_catalog.pg_roles r ON r.oid = d.refobjid
CROSS JOIN LATERAL pg_catalog.aclexplode(CASE d.classid
  WHEN 'pg_catalog.pg_class'::pg_catalog.regclass THEN CASE WHEN d.objsubid = 0
    THEN (SELECT c.relacl FROM pg_catalog.pg_class c WHERE c.oid = d.objid)
    ELSE (SELECT t.attacl FROM pg_catalog.pg_attribute t
          WHERE t.attrelid = d.objid AND t.attnum = d.objsubid) END
  WHEN 'pg_catalog.pg_namespace'::pg_catalog.regclass
    THEN (SELECT n.nspacl FROM pg_catalog.pg_namespace n WHERE n.oid = d.objid)
  WHEN 'pg_catalog.pg_proc'::pg_catalog.regclass
    THEN (SELECT p.proacl FROM pg_catalog.pg_proc p WHERE p.oid = d.objid)
  WHEN 'pg_catalog.pg_database'::pg_catalog.regclass
    THEN (SELECT b.datacl FROM pg_catalog.pg_database b WHERE b.oid = d.objid)
  END) a
WHERE d.refclassid = 'pg_catalog.pg_authid'::pg_catalog.regclass AND d.deptype = 'a'
  AND r.rolname = ANY($1)
  AND d.dbid IN (0, (SELECT oid FROM pg_catalog.pg_database
                     WHERE datname = pg_catalog.current_database()))
  AND a.grantee = d.refobjid
  AND a.grantor <> (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
ORDER BY 1, 2, 3`

// ForeignGrants lists the privileges roles hold in q's database (and on
// shared objects) from grantors other than q's current_user.
func ForeignGrants(ctx context.Context, q Querier, roles []string) ([]Residue, error) {
	rows, err := q.Query(ctx, residueSQL, roles)
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading foreign grants: %w", err)
	}
	defer rows.Close()
	var out []Residue
	for rows.Next() {
		var r Residue
		if err := rows.Scan(&r.Role, &r.Object, &r.Grantor); err != nil {
			return nil, fmt.Errorf("agentguard: reading foreign grants: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
