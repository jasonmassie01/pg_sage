package agentposture

import (
	"context"
	"fmt"
	"strings"
)

func init() { Register(ap06{}) }

// ap06 reports views in exposed schemas over tables with row-level
// security that run as their owner (no security_invoker): the base
// tables' policies are checked against the view's owner, not the caller.
// PostgreSQL 15 added security_invoker; on PostgreSQL 14 every such view
// is reported with "no security_invoker available".
type ap06 struct{}

// AP-06 arms.
const (
	armSecurityInvoker   = "security_invoker"
	armNoSecurityInvoker = "no_security_invoker"
)

func (ap06) Spec() Spec {
	return Spec{ID: "AP-06", Title: "Owner-rights views over RLS tables",
		Severity: Warning, Arms: []Arm{
			{Name: armSecurityInvoker, MinVersion: 150000,
				SkipReason: "security_invoker views need PostgreSQL 15+"},
			{Name: armNoSecurityInvoker, MaxVersion: 150000,
				SkipReason: "PostgreSQL 15+ has security_invoker"},
		}}
}

var ap06SQL = Statement("AP-06", `SELECT n.nspname::text, v.relname::text,
  array_agg(DISTINCT bn.nspname::text || '.' || b.relname::text ORDER BY
    bn.nspname::text || '.' || b.relname::text)
FROM pg_catalog.pg_class v
JOIN pg_catalog.pg_namespace n ON n.oid = v.relnamespace
JOIN pg_catalog.pg_rewrite rw ON rw.ev_class = v.oid
JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_rewrite'::pg_catalog.regclass
  AND d.objid = rw.oid AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
JOIN pg_catalog.pg_class b ON b.oid = d.refobjid AND b.relkind IN ('r', 'p')
  AND b.relrowsecurity AND b.oid <> v.oid
JOIN pg_catalog.pg_namespace bn ON bn.oid = b.relnamespace
WHERE v.relkind = 'v' AND `+userSchemaFilter+`
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend e
    WHERE e.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND e.objid = v.oid
      AND e.objsubid = 0 AND e.deptype = 'e')
  AND EXISTS (SELECT 1 FROM pg_catalog.aclexplode(
        COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) u
      WHERE u.privilege_type = 'USAGE' AND u.grantee = ANY($1::oid[]))
  AND NOT COALESCE((SELECT o.option_value IN ('true', 'on', '1', 'yes')
        FROM pg_catalog.pg_options_to_table(v.reloptions) o
        WHERE o.option_name = 'security_invoker'), false)
GROUP BY n.nspname, v.oid, v.relname
ORDER BY n.nspname, v.relname
LIMIT $2`)

func (ap06) Detect(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, ap06SQL, in.Env.ExposedOIDs(), maxRows)
	if err != nil {
		return nil, fmt.Errorf("read owner-rights views: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var schema, name string
		var bases []string
		if err := rows.Scan(&schema, &name, &bases); err != nil {
			return nil, fmt.Errorf("read owner-rights views: %w", err)
		}
		out = append(out, ap06Finding(in, schema, name, bases))
	}
	return out, rows.Err()
}

func ap06Finding(in Input, schema, name string, bases []string) Finding {
	obj := schema + "." + name
	qn := QualifiedName(schema, name)
	tables := strings.Join(bases, ", ")
	f := Finding{Severity: Warning, ObjectType: "view", Object: obj,
		Detail: fmt.Sprintf("View %s in an exposed schema reads %s, which use row-level "+
			"security, but runs as its owner: the policies are checked against the "+
			"view's owner, not the user who queries it.", obj, tables),
		Evidence: []Evidence{{Source: "pg_class.reloptions", Ref: obj,
			Detail: "no security_invoker; base tables with RLS: " + tables}}}
	if in.Arm(armSecurityInvoker) {
		f.Title = fmt.Sprintf("View %s bypasses row-level security of its tables", obj)
		f.Recommendation = "Make the view a security_invoker view so the caller's " +
			"policies apply."
		f.FixScript = "ALTER VIEW " + qn + " SET (security_invoker = true);"
		f.Caveat = "Callers then need their own privileges on the base tables."
		return f
	}
	f.Title = fmt.Sprintf("View %s bypasses row-level security of its tables; "+
		"no security_invoker available (PostgreSQL 14)", obj)
	f.Recommendation = "Revoke the view from exposed roles, or replace it with a " +
		"function that checks the caller; upgrading to PostgreSQL 15+ allows " +
		"security_invoker views."
	f.FixScript = "-- PostgreSQL 14 has no security_invoker views.\n" +
		"REVOKE ALL ON " + qn + " FROM PUBLIC;\n" + revokeExposed(in.Env, qn)
	return f
}

// revokeExposed revokes qn from every configured or Supabase exposed role.
func revokeExposed(env Env, qn string) string {
	var lines []string
	for _, r := range env.Exposed {
		if r.OID != PublicOID {
			lines = append(lines, fmt.Sprintf("REVOKE ALL ON %s FROM %s;", qn,
				grantTarget(r)))
		}
	}
	return strings.Join(lines, "\n")
}
