package broker

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// relation is a FROM-clause name resolved on the broker's search path.
type relation struct {
	Schema, Name string
	OID          uint32 // 0 = did not resolve
	Kind         string
}

// resolveRelationsSQL resolves each name the way the broker's session
// will: a qualified name as written, an unqualified one in pg_catalog,
// then the profile schemas, in order (pg_temp is never reachable: the
// transaction is read-only and agents create nothing).
const resolveRelationsSQL = `/* pg_sage agent_query v1 */
SELECT r.ord, coalesce(n.nspname, r.schema), r.name, coalesce(c.oid, 0)::oid,
	coalesce(c.relkind::text, '')
FROM ROWS FROM (pg_catalog.unnest($1::text[]), pg_catalog.unnest($2::text[]))
	WITH ORDINALITY AS r(schema, name, ord)
LEFT JOIN LATERAL (
	SELECT pg_catalog.to_regclass(pg_catalog.format('%I.%I', s.nsp, r.name)) AS oid
	FROM pg_catalog.unnest(CASE WHEN r.schema = '' THEN $3::text[]
		ELSE ARRAY[r.schema] END) WITH ORDINALITY AS s(nsp, pos)
	WHERE pg_catalog.to_regclass(pg_catalog.format('%I.%I', s.nsp, r.name)) IS NOT NULL
	ORDER BY s.pos LIMIT 1
) hit ON true
LEFT JOIN pg_catalog.pg_class c ON c.oid = hit.oid
LEFT JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
ORDER BY r.ord`

// resolveRelations resolves the statement's relations, skipping CTE
// references, with pg_sage's own pool (the catalog is the same).
func resolveRelations(ctx context.Context, pool *pgxpool.Pool, q sqlast.ReadQuery,
	searchPath []string) ([]relation, error) {
	ctes := map[string]bool{}
	for _, n := range q.CTENames {
		ctes[n] = true
	}
	var schemas, names []string
	for _, r := range q.Relations {
		if r.Schema == "" && ctes[r.Name] {
			continue
		}
		schemas, names = append(schemas, r.Schema), append(names, r.Name)
	}
	if len(names) == 0 {
		return nil, nil
	}
	path := append([]string{"pg_catalog"}, searchPath...)
	rows, err := pool.Query(ctx, resolveRelationsSQL, schemas, names, path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolving the query's relations: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	var out []relation
	for rows.Next() {
		var ord int64
		var r relation
		if err := rows.Scan(&ord, &r.Schema, &r.Name, &r.OID, &r.Kind); err != nil {
			return nil, fmt.Errorf("%w: reading resolved relations: %v", ErrUnavailable, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: resolving the query's relations: %v", ErrUnavailable, err)
	}
	return out, nil
}

// grantedSchemas are the schemas of the search path (spec §6.8, "profile
// schemas"): those whose USAGE the broker role holds by a grant naming it,
// sorted. A schema it reaches only through PUBLIC (public, by default) is
// left out, so a name the agent uses unqualified never resolves to an
// object planted where anyone may create. System and pg_sage schemas are
// never on it.
func grantedSchemas(ctx context.Context, pool *pgxpool.Pool, role string) ([]string,
	error) {
	rows, err := pool.Query(ctx, `/* pg_sage agent_query v1 */
		SELECT DISTINCT n.nspname::text
		FROM pg_catalog.pg_namespace n, pg_catalog.aclexplode(n.nspacl) a
		WHERE n.nspacl IS NOT NULL AND a.privilege_type = 'USAGE'
		  AND a.grantee = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1)
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage', 'sage_guard')
		  AND n.nspname !~ '^pg_'
		ORDER BY 1`, role)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the agent's schemas: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("%w: reading the agent's schemas: %v", ErrUnavailable, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// objectsOf is the gate's view of what the statement touches (D5).
func objectsOf(rels []relation) []decide.Object {
	out := make([]decide.Object, 0, len(rels))
	for _, r := range rels {
		out = append(out, decide.Object{Schema: r.Schema, Relation: r.Name})
	}
	return out
}

// attributeNames reads the live column names of relids.
func attributeNames(ctx context.Context, pool *pgxpool.Pool, relids []uint32) (
	map[uint32]map[int16]string, error) {
	out := map[uint32]map[int16]string{}
	if len(relids) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx, `/* pg_sage agent_query v1 */
		SELECT attrelid, attnum, attname FROM pg_catalog.pg_attribute
		WHERE attrelid = ANY($1::oid[]) AND attnum > 0 AND NOT attisdropped`, relids)
	if err != nil {
		return nil, fmt.Errorf("%w: reading column names: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	for rows.Next() {
		var rel uint32
		var num int16
		var name string
		if err := rows.Scan(&rel, &num, &name); err != nil {
			return nil, fmt.Errorf("%w: reading column names: %v", ErrUnavailable, err)
		}
		if out[rel] == nil {
			out[rel] = map[int16]string{}
		}
		out[rel][num] = name
	}
	return out, rows.Err()
}

// typeNames maps type OIDs to their SQL names.
func typeNames(ctx context.Context, pool *pgxpool.Pool, oids []uint32) (map[uint32]string,
	error) {
	out := map[uint32]string{}
	rows, err := pool.Query(ctx, `/* pg_sage agent_query v1 */
		SELECT oid, pg_catalog.format_type(oid, NULL) FROM pg_catalog.pg_type
		WHERE oid = ANY($1::oid[])`, oids)
	if err != nil {
		return nil, fmt.Errorf("%w: reading type names: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var name string
		if err := rows.Scan(&oid, &name); err != nil {
			return nil, fmt.Errorf("%w: reading type names: %v", ErrUnavailable, err)
		}
		out[oid] = name
	}
	return out, rows.Err()
}

// viewDependencySQL lists the relations and columns a view's rewrite rule
// reads (attnum 0: the relation as a whole).
const viewDependencySQL = `/* pg_sage agent_query v1 */
SELECT DISTINCT d.refobjid::oid, d.refobjsubid::int2, c.relkind::text
FROM pg_catalog.pg_rewrite r
JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_rewrite'::regclass
	AND d.objid = r.oid AND d.refclassid = 'pg_catalog.pg_class'::regclass
JOIN pg_catalog.pg_class c ON c.oid = d.refobjid
WHERE r.ev_class = $1::oid AND d.refobjid <> $1::oid`

type dependency struct {
	relid  uint32
	attnum int16
	kind   string
}

func viewDependencies(ctx context.Context, pool *pgxpool.Pool, view uint32) ([]dependency,
	error) {
	rows, err := pool.Query(ctx, viewDependencySQL, view)
	if err != nil {
		return nil, fmt.Errorf("%w: reading view dependencies: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	var out []dependency
	for rows.Next() {
		var d dependency
		if err := rows.Scan(&d.relid, &d.attnum, &d.kind); err != nil {
			return nil, fmt.Errorf("%w: reading view dependencies: %v", ErrUnavailable, err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
