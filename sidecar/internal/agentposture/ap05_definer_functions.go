package agentposture

import (
	"context"
	"fmt"
)

func init() { Register(ap05{}) }

// ap05 reports SECURITY DEFINER functions that exposed roles (or PUBLIC,
// the default) can execute and whose search_path is not pinned: a caller
// who can create objects on the path can make the function run their code
// with its owner's rights.
type ap05 struct{}

func (ap05) Spec() Spec {
	return Spec{ID: "AP-05", Title: "Definer functions without a pinned search_path",
		Severity: Warning}
}

var ap05SQL = Statement("AP-05", `SELECT n.nspname::text, p.proname::text,
  pg_catalog.pg_get_function_identity_arguments(p.oid), p.prokind::text,
  array_agg(DISTINCT a.grantee), `+schemaUsageGrantees+`
FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
CROSS JOIN LATERAL pg_catalog.aclexplode(
  COALESCE(p.proacl, pg_catalog.acldefault('f', p.proowner))) a
WHERE p.prosecdef AND `+userSchemaFilter+`
  AND a.privilege_type = 'EXECUTE' AND a.grantee = ANY($1::oid[])
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.unnest(p.proconfig) s
                  WHERE s LIKE 'search_path=%')
GROUP BY n.oid, n.nspname, n.nspacl, n.nspowner, p.oid, p.proname, p.prokind
ORDER BY n.nspname, p.proname
LIMIT $2`)

func (ap05) Detect(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, ap05SQL, in.Env.ExposedOIDs(), maxRows)
	if err != nil {
		return nil, fmt.Errorf("read definer functions: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var schema, name, args, kind string
		var grantees, usage []uint32
		if err := rows.Scan(&schema, &name, &args, &kind, &grantees, &usage); err != nil {
			return nil, fmt.Errorf("read definer functions: %w", err)
		}
		reach := reachingRoles(in.Env, grantees, usage)
		if len(reach) == 0 {
			continue
		}
		out = append(out, ap05Finding(schema, name, args, kind, reach, grantees))
	}
	return out, rows.Err()
}

func ap05Finding(schema, name, args, kind string, reach []Role,
	grantees []uint32) Finding {
	obj := fmt.Sprintf("%s.%s(%s)", schema, name, args)
	sig := fmt.Sprintf("%s(%s)", QualifiedName(schema, name), args)
	what := "FUNCTION"
	if kind == "p" {
		what = "PROCEDURE"
	}
	revoke, via := "", ""
	if containsOID(grantees, PublicOID) {
		revoke = fmt.Sprintf("\n-- REVOKE EXECUTE ON %s %s FROM PUBLIC;", what, sig)
		via = " (EXECUTE is granted to PUBLIC)"
	}
	for _, r := range reach {
		if r.OID != PublicOID && containsOID(grantees, r.OID) {
			revoke += fmt.Sprintf("\n-- REVOKE EXECUTE ON %s %s FROM %s;", what, sig,
				grantTarget(r))
		}
	}
	return Finding{Severity: Warning, ObjectType: "function", Object: obj,
		Title: fmt.Sprintf("Definer function %s has no pinned search_path", obj),
		Detail: fmt.Sprintf("%s runs with its owner's rights (SECURITY DEFINER), "+
			"%s can execute it%s, and its search_path is not set. A caller who can "+
			"create objects on the search path can make it run their code as the owner.",
			obj, roleList(reach), via),
		Recommendation: "Pin the function's search_path, and revoke EXECUTE from roles " +
			"that do not need it.",
		FixScript: fmt.Sprintf("ALTER %s %s SET search_path = pg_catalog, pg_temp;\n"+
			"-- and, if exposed roles do not need it:%s", what, sig, revoke),
		Caveat: "With a pinned search_path the body must schema-qualify the objects it " +
			"uses; test the function afterwards.",
		Evidence: []Evidence{{Source: "pg_proc", Ref: obj,
			Detail: "prosecdef, no search_path in proconfig"}}}
}
