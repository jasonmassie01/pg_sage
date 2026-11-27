package agentposture

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
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

// ap05Item is one exposed definer function.
type ap05Item struct {
	schema, name, args, kind string
	reach                    []Role
	grantees                 []uint32
}

func (ap05) Detect(ctx context.Context, in Input) ([]Finding, error) {
	lim := in.capRows("functions", maxRows)
	rows, err := in.Q.Query(ctx, ap05SQL, in.Env.ReachOIDs(), lim.fetch())
	if err != nil {
		return nil, fmt.Errorf("read definer functions: %w", err)
	}
	defer rows.Close()
	in.noteVia()
	var found []ap05Item
	for rows.Next() && !lim.past() {
		var it ap05Item
		var usage []uint32
		if err := rows.Scan(&it.schema, &it.name, &it.args, &it.kind, &it.grantees,
			&usage); err != nil {
			return nil, fmt.Errorf("read definer functions: %w", err)
		}
		if it.reach = reachingRoles(in.Env, it.grantees, usage); len(it.reach) > 0 {
			found = append(found, it)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read definer functions: %w", err)
	}
	return groupBySchema(found, func(it ap05Item) string { return it.schema },
		func(it ap05Item) Finding { return ap05Finding(in.Env, it) },
		func(schema string, items []ap05Item) Finding {
			return ap05SchemaFinding(in.Env, schema, items)
		}), nil
}

// ap05PinAll pins the search_path of every SECURITY DEFINER routine of
// schema s that has none (a DO block body).
func ap05PinAll(s string) string {
	return `DECLARE r record;
BEGIN
  FOR r IN SELECT p.oid::pg_catalog.regprocedure AS sig, p.prokind
    FROM pg_catalog.pg_proc p
    WHERE p.pronamespace = ` + schemaLiteral(s) + ` AND p.prosecdef
      AND NOT EXISTS (SELECT 1 FROM pg_catalog.unnest(p.proconfig) c
                      WHERE c LIKE 'search_path=%')
  LOOP
    EXECUTE pg_catalog.format('ALTER %s %s SET search_path = pg_catalog, pg_temp',
      CASE r.prokind WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END, r.sig);
  END LOOP;
END`
}

// ap05SchemaFinding is one finding for a schema with more than
// groupThreshold exposed definer functions; its fix pins them all.
func ap05SchemaFinding(env Env, schema string, items []ap05Item) Finding {
	var reach, targets []Role
	var ps []Path
	objs := make([]string, len(items))
	ev := make([]Evidence, len(items))
	for i, it := range items {
		objs[i] = fmt.Sprintf("%s.%s(%s)", it.schema, it.name, it.args)
		ev[i] = Evidence{Source: "pg_proc", Ref: objs[i],
			Detail: "prosecdef, no search_path in proconfig"}
		reach = mergeRoles(reach, it.reach)
		targets = mergeRoles(targets, revokeTargets(env, it.reach, it.grantees))
		ps = append(ps, env.paths(it.reach, it.grantees)...)
	}
	fix := []string{"-- Pins the search_path of every SECURITY DEFINER routine in schema " +
		schema + " that has none.", "DO " + dollarQuoted(ap05PinAll(schema)) + ";",
		"-- and, if exposed roles do not need them:"}
	for _, r := range targets {
		fix = append(fix, "-- REVOKE EXECUTE ON ALL ROUTINES IN SCHEMA "+sqlident.Ident(schema)+
			" FROM "+grantTarget(r)+";")
	}
	return Finding{Severity: Warning, ObjectType: "schema", Object: schema,
		Title: fmt.Sprintf("%d definer functions in schema %s have no pinned search_path",
			len(items), schema),
		Detail: fmt.Sprintf("%d SECURITY DEFINER functions in %s that %s can execute "+
			"have no search_path set: %s. A caller who can create objects on the search "+
			"path can make them run their code as the owner.", len(items), schema,
			roleList(reach), listSome(objs, 10)),
		Recommendation: "Pin the functions' search_path, and revoke EXECUTE from roles " +
			"that do not need them.",
		FixScript: strings.Join(fix, "\n"),
		Caveat: joinCaveats("With a pinned search_path the bodies must schema-qualify the "+
			"objects they use; the script pins every definer routine of the schema without "+
			"one, so test them afterwards.", groupCaveat(ps)),
		Evidence: groupEvidence("pg_proc", schema, "definer functions", ev)}
}

// ap05Finding reports one definer function. Its commented REVOKEs name
// PUBLIC, the exposed roles granted EXECUTE directly and the inherited
// groups granted it; for a group it also offers ending the membership.
func ap05Finding(env Env, it ap05Item) Finding {
	obj := fmt.Sprintf("%s.%s(%s)", it.schema, it.name, it.args)
	sig := fmt.Sprintf("%s(%s)", sqlident.Qualified(it.schema, it.name), it.args)
	what := "FUNCTION"
	if it.kind == "p" {
		what = "PROCEDURE"
	}
	ps := env.paths(it.reach, it.grantees)
	via := viaText(ps)
	if containsOID(it.grantees, PublicOID) {
		via = " (EXECUTE is granted to PUBLIC)" + via
	}
	revoke := ""
	for _, r := range revokeTargets(env, it.reach, it.grantees) {
		revoke += fmt.Sprintf("\n-- REVOKE EXECUTE ON %s %s FROM %s;", what, sig,
			grantTarget(r))
	}
	for _, line := range membershipFix(ps) {
		revoke += "\n" + line
	}
	return Finding{Severity: Warning, ObjectType: "function", Object: obj,
		Title: fmt.Sprintf("Definer function %s has no pinned search_path", obj),
		Detail: fmt.Sprintf("%s runs with its owner's rights (SECURITY DEFINER), "+
			"%s can execute it%s, and its search_path is not set. A caller who can "+
			"create objects on the search path can make it run their code as the owner.",
			obj, roleList(it.reach), via),
		Recommendation: "Pin the function's search_path, and revoke EXECUTE from roles " +
			"that do not need it.",
		FixScript: fmt.Sprintf("ALTER %s %s SET search_path = pg_catalog, pg_temp;\n"+
			"-- and, if exposed roles do not need it:%s", what, sig, revoke),
		Caveat: joinCaveats("With a pinned search_path the body must schema-qualify the "+
			"objects it uses; test the function afterwards.", groupCaveat(ps)),
		Evidence: append([]Evidence{{Source: "pg_proc", Ref: obj,
			Detail: "prosecdef, no search_path in proconfig"}}, viaEvidence(obj, ps)...)}
}
