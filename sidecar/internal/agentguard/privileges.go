package agentguard

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Privilege is one privilege on one object of a database: a relation (a
// table, view, materialized view, foreign or partitioned table, or a
// sequence), a column (Attnum > 0), a schema, a function or the database.
type Privilege struct {
	Kind      string `json:"kind"` // database | schema | relation | column | function
	OID       uint32 `json:"oid"`
	Attnum    int16  `json:"attnum,omitempty"`
	Name      string `json:"name"`
	Privilege string `json:"privilege"`
}

type privKey struct {
	kind   string
	oid    uint32
	attnum int16
	priv   string
}

func (p Privilege) key() privKey { return privKey{p.Kind, p.OID, p.Attnum, p.Privilege} }

// userObjects excludes the system schemas from every privilege read
// (G1-02: "excluding pg_catalog and information_schema").
const userSchema = `n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname !~ '^pg_(toast|temp_|toast_temp_)'`

// relationKinds are the relkinds privileges are read for.
const relationKinds = `c.relkind IN ('r', 'v', 'm', 'f', 'p', 'S')`

// baselineSQL is what PUBLIC may do in this database, from the ACLs (or
// each owner's default), plus default privileges for PUBLIC (P4).
const baselineSQL = `
SELECT 'database', d.oid, 0::int2, d.datname::text, a.privilege_type
FROM pg_catalog.pg_database d,
  pg_catalog.aclexplode(COALESCE(d.datacl, pg_catalog.acldefault('d', d.datdba))) a
WHERE d.datname = pg_catalog.current_database() AND a.grantee = 0
UNION ALL
SELECT 'schema', n.oid, 0::int2, n.nspname::text, a.privilege_type
FROM pg_catalog.pg_namespace n,
  pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
WHERE ` + userSchema + ` AND a.grantee = 0
UNION ALL
SELECT 'relation', c.oid, 0::int2, pg_catalog.format('%I.%I', n.nspname, c.relname),
  a.privilege_type
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace,
  pg_catalog.aclexplode(COALESCE(c.relacl, pg_catalog.acldefault(
    CASE WHEN c.relkind = 'S' THEN 's' ELSE 'r' END::"char", c.relowner))) a
WHERE ` + relationKinds + ` AND ` + userSchema + ` AND a.grantee = 0
UNION ALL
SELECT 'column', c.oid, t.attnum, pg_catalog.format('%I.%I.%I', n.nspname, c.relname,
  t.attname), a.privilege_type
FROM pg_catalog.pg_attribute t
JOIN pg_catalog.pg_class c ON c.oid = t.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace,
  pg_catalog.aclexplode(t.attacl) a
WHERE t.attacl IS NOT NULL AND t.attnum > 0 AND NOT t.attisdropped
  AND ` + userSchema + ` AND a.grantee = 0
UNION ALL
SELECT 'function', p.oid, 0::int2, p.oid::pg_catalog.regprocedure::text, a.privilege_type
FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace,
  pg_catalog.aclexplode(COALESCE(p.proacl, pg_catalog.acldefault('f', p.proowner))) a
WHERE ` + userSchema + ` AND a.grantee = 0
UNION ALL
SELECT 'default_acl', d.oid, 0::int2,
  pg_catalog.format('%s for %s in %s', d.defaclobjtype,
    pg_catalog.pg_get_userbyid(d.defaclrole),
    COALESCE((SELECT nspname FROM pg_catalog.pg_namespace WHERE oid = d.defaclnamespace),
      'every schema')), a.privilege_type
FROM pg_catalog.pg_default_acl d, pg_catalog.aclexplode(d.defaclacl) a
WHERE a.grantee = 0`

// RecordPublicBaseline replaces the database's recorded PUBLIC baseline
// with the current one (§6.6 P4) and returns how many privileges it holds.
func RecordPublicBaseline(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `/* pg_sage guard_baseline v1 */
			DELETE FROM sage.guard_public_baseline`); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `/* pg_sage guard_baseline v1 */
			INSERT INTO sage.guard_public_baseline (object_kind, object_oid, attnum,
				object_name, privilege) `+baselineSQL+` ON CONFLICT DO NOTHING`)
		n = int(tag.RowsAffected())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("agentguard: recording the PUBLIC baseline: %w", err)
	}
	return n, nil
}

// ErrNoBaseline is a privilege check on a database whose preflight never
// recorded the PUBLIC baseline; the check fails closed.
var ErrNoBaseline = errors.New("agentguard: no PUBLIC baseline recorded; run the preflight")

// tablePrivileges are the relation privileges read for a role; PostgreSQL
// 17 adds MAINTAIN.
func tablePrivileges(version int) []string {
	out := []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES",
		"TRIGGER"}
	if version >= 170000 {
		out = append(out, "MAINTAIN")
	}
	return out
}

// effectiveSQL is what role $1 may do, from has_*_privilege over the
// catalog outside the system schemas. Column privileges are read only
// where column ACLs exist and the table-level privilege is absent.
const effectiveSQL = `/* pg_sage guard_effective_privileges v1 */
SELECT 'database', d.oid, 0::int2, d.datname::text, p
FROM pg_catalog.pg_database d, unnest(ARRAY['CONNECT', 'CREATE', 'TEMPORARY']) p
WHERE d.datname = pg_catalog.current_database()
  AND pg_catalog.has_database_privilege($1, d.oid, p)
UNION ALL
SELECT 'schema', n.oid, 0::int2, n.nspname::text, p
FROM pg_catalog.pg_namespace n, unnest(ARRAY['USAGE', 'CREATE']) p
WHERE ` + userSchema + ` AND pg_catalog.has_schema_privilege($1, n.oid, p)
UNION ALL
SELECT 'relation', c.oid, 0::int2, pg_catalog.format('%I.%I', n.nspname, c.relname), p
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace,
  unnest(CASE WHEN c.relkind = 'S' THEN ARRAY['USAGE', 'SELECT', 'UPDATE']
         ELSE $2::text[] END) p
WHERE ` + relationKinds + ` AND ` + userSchema + `
  AND CASE WHEN c.relkind = 'S' THEN pg_catalog.has_sequence_privilege($1, c.oid, p)
      ELSE pg_catalog.has_table_privilege($1, c.oid, p) END
UNION ALL
SELECT 'column', c.oid, t.attnum, pg_catalog.format('%I.%I.%I', n.nspname, c.relname,
  t.attname), p
FROM pg_catalog.pg_attribute t
JOIN pg_catalog.pg_class c ON c.oid = t.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace,
  unnest(ARRAY['SELECT', 'INSERT', 'UPDATE', 'REFERENCES']) p
WHERE t.attacl IS NOT NULL AND t.attnum > 0 AND NOT t.attisdropped
  AND ` + userSchema + ` AND c.relkind <> 'S'
  AND pg_catalog.has_column_privilege($1, c.oid, t.attnum, p)
  AND NOT pg_catalog.has_table_privilege($1, c.oid, p)
UNION ALL
SELECT 'function', f.oid, 0::int2, f.oid::pg_catalog.regprocedure::text, 'EXECUTE'
FROM pg_catalog.pg_proc f JOIN pg_catalog.pg_namespace n ON n.oid = f.pronamespace
WHERE ` + userSchema + ` AND pg_catalog.has_function_privilege($1, f.oid, 'EXECUTE')`

// EffectivePrivileges reads what role may do in q's database.
func EffectivePrivileges(ctx context.Context, q Querier, role string) ([]Privilege, error) {
	v, err := serverVersion(ctx, q)
	if err != nil {
		return nil, err
	}
	return readPrivileges(ctx, q, effectiveSQL, role, tablePrivileges(v))
}

func readPrivileges(ctx context.Context, q Querier, sql string, args ...any) ([]Privilege,
	error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading privileges: %w", err)
	}
	defer rows.Close()
	var out []Privilege
	for rows.Next() {
		var p Privilege
		if err := rows.Scan(&p.Kind, &p.OID, &p.Attnum, &p.Name, &p.Privilege); err != nil {
			return nil, fmt.Errorf("agentguard: reading privileges: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PrivilegeReport compares a role's effective privileges with what Guard
// allows it (G1-02).
type PrivilegeReport struct {
	Role      string      `json:"role"`
	Effective int         `json:"effective"`
	Excess    []Privilege `json:"excess"`
}

// CheckEffectivePrivileges verifies that role's effective privileges in
// pool's database are exactly within its Guard grants, the CONNECT its
// role contract grants and the PUBLIC baseline recorded at preflight —
// never the launching admin's (G1-02, SAFE-ID-02). Excess lists the rest.
func CheckEffectivePrivileges(ctx context.Context, pool *pgxpool.Pool, role string,
	grants []Privilege) (PrivilegeReport, error) {
	baseline, err := readPrivileges(ctx, pool, `/* pg_sage guard_baseline v1 */
		SELECT object_kind, object_oid, attnum, object_name, privilege
		FROM sage.guard_public_baseline`)
	if err != nil {
		return PrivilegeReport{}, err
	}
	if len(baseline) == 0 {
		return PrivilegeReport{}, ErrNoBaseline
	}
	effective, err := EffectivePrivileges(ctx, pool, role)
	if err != nil {
		return PrivilegeReport{}, err
	}
	allowed := map[privKey]bool{}
	for _, p := range append(baseline, grants...) {
		allowed[p.key()] = true
	}
	rep := PrivilegeReport{Role: role, Effective: len(effective), Excess: []Privilege{}}
	for _, p := range effective {
		connect := p.Kind == "database" && p.Privilege == "CONNECT"
		if !allowed[p.key()] && !connect {
			rep.Excess = append(rep.Excess, p)
		}
	}
	sort.Slice(rep.Excess, func(i, j int) bool {
		a, b := rep.Excess[i], rep.Excess[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Privilege < b.Privilege
	})
	return rep, nil
}
