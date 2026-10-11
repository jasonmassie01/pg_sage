package agentposture

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func init() { Register(ap03{}) }

// ap03 reports tables and views exposed roles can use without row-level
// security: a table with RLS disabled granted to an exposed role (or to
// PUBLIC), and a view granted to one that runs as its owner over a table
// with RLS disabled. Read through aclexplode of relacl (or acldefault);
// the exposed role must also hold USAGE on the schema.
type ap03 struct{}

func (ap03) Spec() Spec {
	return Spec{ID: "AP-03", Title: "Tables reachable by exposed roles without RLS",
		Severity: Critical}
}

// ap03SQL reads each table or view that grants a data privilege to an
// exposed role or PUBLIC: who holds what, the schema's USAGE grantees and,
// for views that run as their owner, the base tables without RLS.
var ap03SQL = Statement("AP-03", `SELECT n.nspname::text, c.relname::text, c.relkind::text,
  array_agg(DISTINCT a.grantee), array_agg(DISTINCT a.grantee::text || ':' ||
    a.privilege_type ORDER BY a.grantee::text || ':' || a.privilege_type),
  `+schemaUsageGrantees+`,
  CASE WHEN c.relkind = 'v' THEN ARRAY(
    SELECT DISTINCT bn.nspname::text || '.' || b.relname::text
    FROM pg_catalog.pg_rewrite rw
    JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_rewrite'::pg_catalog.regclass
      AND d.objid = rw.oid AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
    JOIN pg_catalog.pg_class b ON b.oid = d.refobjid AND b.relkind IN ('r', 'p')
      AND NOT b.relrowsecurity
    JOIN pg_catalog.pg_namespace bn ON bn.oid = b.relnamespace
    WHERE rw.ev_class = c.oid AND b.oid <> c.oid ORDER BY 1) END
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL pg_catalog.aclexplode(
  COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))) a
WHERE c.relkind IN ('r', 'p', 'v') AND `+userSchemaFilter+` AND `+notExtensionMember+`
  AND (c.relacl IS NOT NULL OR c.relowner = ANY($1::oid[]))
  AND a.grantee = ANY($1::oid[])
  AND a.privilege_type IN ('SELECT', 'INSERT', 'UPDATE', 'DELETE', 'TRUNCATE')
  AND ((c.relkind IN ('r', 'p') AND NOT c.relrowsecurity)
    OR (c.relkind = 'v' AND NOT COALESCE((SELECT o.option_value IN ('true', 'on', '1', 'yes')
          FROM pg_catalog.pg_options_to_table(c.reloptions) o
          WHERE o.option_name = 'security_invoker'), false)))
GROUP BY n.oid, n.nspname, n.nspacl, n.nspowner, c.oid, c.relname, c.relkind
ORDER BY n.nspname, c.relname
LIMIT $2`)

type ap03Row struct {
	schema, name, kind string
	grantees, usage    []uint32
	grants             []string // "grantee_oid:PRIVILEGE"
	bases              []string // views: base tables without RLS
}

func (ap03) Detect(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, ap03SQL, in.Env.ExposedOIDs(), maxRows)
	if err != nil {
		return nil, fmt.Errorf("read relation grants to exposed roles: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var r ap03Row
		if err := rows.Scan(&r.schema, &r.name, &r.kind, &r.grantees, &r.grants, &r.usage,
			&r.bases); err != nil {
			return nil, fmt.Errorf("read relation grants to exposed roles: %w", err)
		}
		reach := reachingRoles(in.Env, r.grantees, r.usage)
		if len(reach) == 0 || (r.kind == "v" && len(r.bases) == 0) {
			continue
		}
		out = append(out, ap03Finding(in.Env, r, reach))
	}
	return out, rows.Err()
}

func ap03Finding(env Env, r ap03Row, reach []Role) Finding {
	obj := r.schema + "." + r.name
	qn := QualifiedName(r.schema, r.name)
	granted := grantText(env, r.grants)
	f := Finding{Severity: Critical, Object: obj,
		Evidence: []Evidence{{Source: "pg_class.relacl", Ref: obj, Detail: granted},
			{Source: "pg_namespace.nspacl", Ref: r.schema,
				Detail: "USAGE reaches " + roleList(reach)}}}
	if r.kind == "v" {
		f.ObjectType = "view"
		f.Title = fmt.Sprintf("View %s exposes tables without row-level security", obj)
		f.Detail = fmt.Sprintf("%s is granted %s; reachable by %s. It runs as its owner "+
			"over %s, which has row-level security disabled, so every row is visible.",
			obj, granted, roleList(reach), strings.Join(r.bases, ", "))
		f.Recommendation = "Revoke the view from exposed roles, or enable RLS with " +
			"policies on its tables and make it a security_invoker view (PostgreSQL 15+)."
		f.FixScript = revokeAll(qn, reach) + "\n-- or, on PostgreSQL 15+, after enabling " +
			"RLS on " + strings.Join(r.bases, ", ") + ":\n-- ALTER VIEW " + qn +
			" SET (security_invoker = true);"
		return f
	}
	f.ObjectType = "table"
	f.Title = fmt.Sprintf("Table %s is reachable by exposed roles without row-level "+
		"security", obj)
	f.Detail = fmt.Sprintf("%s is granted %s; reachable by %s. Row-level security is "+
		"disabled, so these roles reach every row.", obj, granted, roleList(reach))
	f.Recommendation = "Enable row-level security with policies for each exposed role, " +
		"or revoke the grants if the table is not meant for them."
	f.FixScript = "-- Write the policies first: with RLS on and no policy, every row is " +
		"hidden.\nALTER TABLE " + qn + " ENABLE ROW LEVEL SECURITY;\n-- or, if the " +
		"table is not meant for them:\n" + revokeAll(qn, reach)
	f.Caveat = "Enabling row-level security without a policy hides every row from " +
		"non-owner roles; an application using the table stops seeing data."
	return f
}

func revokeAll(qn string, reach []Role) string {
	var b strings.Builder
	for i, r := range reach {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "REVOKE ALL ON %s FROM %s;", qn, grantTarget(r))
	}
	return b.String()
}

// grantText renders "SELECT, UPDATE to anon; SELECT to PUBLIC".
func grantText(env Env, grants []string) string {
	by := map[string][]string{}
	for _, g := range grants {
		oid, priv, _ := strings.Cut(g, ":")
		name := "role " + oid
		var id uint32
		if _, err := fmt.Sscan(oid, &id); err == nil {
			if r, ok := env.Exposure(id); ok {
				name = r.Name
			}
		}
		by[name] = append(by[name], priv)
	}
	names := make([]string, 0, len(by))
	for n := range by {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = strings.Join(by[n], ", ") + " to " + n
	}
	return strings.Join(parts, "; ")
}
