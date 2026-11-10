package agentposture

import (
	"fmt"
	"slices"
	"strings"
)

// ap06EachView is a DO block body that runs each of stmts, format()
// strings with one %s for the view, on every owner-rights candidate view
// of schema s: its views over tables with row-level security that no
// extension owns.
func ap06EachView(s string, stmts []string) string {
	var run []string
	for _, st := range stmts {
		run = append(run, "    EXECUTE pg_catalog.format("+sqlLiteral(st)+", rel);")
	}
	return `DECLARE rel pg_catalog.regclass;
BEGIN
  FOR rel IN SELECT DISTINCT vw.oid::pg_catalog.regclass FROM pg_catalog.pg_class vw
    JOIN pg_catalog.pg_rewrite rw ON rw.ev_class = vw.oid
    JOIN pg_catalog.pg_depend d
      ON d.classid = 'pg_catalog.pg_rewrite'::pg_catalog.regclass AND d.objid = rw.oid
      AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
    JOIN pg_catalog.pg_class b ON b.oid = d.refobjid AND b.relkind IN ('r', 'p')
      AND b.relrowsecurity AND b.oid <> vw.oid
    WHERE vw.relkind = 'v' AND vw.relnamespace = ` + schemaLiteral(s) + `
      AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend e
        WHERE e.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
          AND e.objid = vw.oid AND e.deptype = 'e')
  LOOP
` + strings.Join(run, "\n") + `
  END LOOP;
END`
}

// ap06SchemaFix is the schema finding's fix: every candidate view made a
// security_invoker view (PostgreSQL 15+), or revoked from PUBLIC, the
// exposed roles and the group roles they inherit where security_invoker
// does not exist.
func ap06SchemaFix(in Input, schema string) string {
	head := "-- Every view in schema " + schema + " over tables with row-level security"
	if in.Arm(armSecurityInvoker) {
		return head + " becomes a security_invoker view.\nDO " +
			dollarQuoted(ap06EachView(schema,
				[]string{"ALTER VIEW %s SET (security_invoker = true)"})) + ";"
	}
	stmts := []string{"REVOKE ALL ON %s FROM PUBLIC"}
	for _, r := range in.Env.revokeAllTargets() {
		target := strings.ReplaceAll(grantTarget(r), "%", "%%")
		stmts = append(stmts, "REVOKE ALL ON %s FROM "+target)
	}
	who := " is revoked from PUBLIC and the exposed roles"
	if len(in.Env.inheritedGroups()) > 0 {
		who += " and the group roles they inherit"
	}
	return "-- PostgreSQL 14 has no security_invoker views.\n" + head + who + ".\nDO " +
		dollarQuoted(ap06EachView(schema, stmts)) + ";"
}

// ap06SchemaFinding is one finding for a schema with more than
// groupThreshold owner-rights views over tables with row-level security.
func ap06SchemaFinding(in Input, schema string, items []ap06Item) Finding {
	var bases []string
	objs := make([]string, len(items))
	ev := make([]Evidence, len(items))
	for i, it := range items {
		objs[i] = it.schema + "." + it.name
		ev[i] = Evidence{Source: "pg_class.reloptions", Ref: objs[i],
			Detail: "no security_invoker; base tables with RLS: " +
				strings.Join(it.bases, ", ")}
		for _, b := range it.bases {
			if !slices.Contains(bases, b) {
				bases = append(bases, b)
			}
		}
	}
	slices.Sort(bases)
	f := Finding{Severity: Warning, ObjectType: "schema", Object: schema,
		Title: fmt.Sprintf("%d views in schema %s bypass row-level security of their "+
			"tables", len(items), schema),
		Detail: fmt.Sprintf("%d views in %s, an exposed schema, read %s, which use "+
			"row-level security, but run as their owner: the policies are checked against "+
			"the views' owner, not the user who queries them. Views: %s.", len(items),
			schema, listSome(bases, 10), listSome(objs, 10)),
		Recommendation: "Make the views security_invoker views so the caller's policies " +
			"apply.",
		FixScript: ap06SchemaFix(in, schema),
		Caveat:    "Callers then need their own privileges on the base tables.",
		Evidence:  groupEvidence("pg_class.reloptions", schema, "views", ev)}
	if !in.Arm(armSecurityInvoker) {
		f.Title += "; no security_invoker available (PostgreSQL 14)"
		f.Recommendation = "Revoke the views from exposed roles, or replace them with " +
			"functions that check the caller; upgrading to PostgreSQL 15+ allows " +
			"security_invoker views."
		f.Caveat = joinCaveats("Applications using the views as these roles lose access.",
			in.Env.inheritedCaveat())
	}
	return f
}
