package agentposture

import (
	"context"
	"fmt"
	"strings"
)

func init() { Register(ap15{}) }

// ap15 reports the PUBLIC baseline (spec §6.6, P4): tables, views and
// foreign tables granted to PUBLIC (one finding per schema, a REVOKE per
// relation) and default privileges that grant PUBLIC on future tables or
// sequences. Every agent role is a member of PUBLIC, so these grants are
// every agent's. Objects that belong to an extension are left to it.
// Warning.
type ap15 struct{}

func (ap15) Spec() Spec {
	return Spec{ID: "AP-15", Title: "PUBLIC table grants and default privileges",
		Severity: Warning}
}

var ap15TablesSQL = Statement("AP-15", `SELECT n.nspname::text, c.relname::text,
  ARRAY(SELECT u.privilege_type::text FROM pg_catalog.aclexplode(c.relacl) u
        WHERE u.grantee = 0 ORDER BY 1)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') AND c.relacl IS NOT NULL
  AND `+apbUserSchemas+`
  AND EXISTS (SELECT 1 FROM pg_catalog.aclexplode(c.relacl) u WHERE u.grantee = 0)
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
    WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = c.oid
      AND d.deptype = 'e')
ORDER BY 1, 2
LIMIT $1`)

var ap15DefaultsSQL = Statement("AP-15", `SELECT pg_catalog.pg_get_userbyid(d.defaclrole)::text,
  COALESCE(n.nspname::text, ''), d.defaclobjtype::text,
  ARRAY(SELECT u.privilege_type::text FROM pg_catalog.aclexplode(d.defaclacl) u
        WHERE u.grantee = 0 ORDER BY 1)
FROM pg_catalog.pg_default_acl d
LEFT JOIN pg_catalog.pg_namespace n ON n.oid = d.defaclnamespace
WHERE d.defaclobjtype IN ('r', 'S')
  AND EXISTS (SELECT 1 FROM pg_catalog.aclexplode(d.defaclacl) u WHERE u.grantee = 0)
ORDER BY 1, 2, 3
LIMIT $1`)

func (ap15) Detect(ctx context.Context, in Input) ([]Finding, error) {
	out, err := publicTableGrants(ctx, in.Q)
	if err != nil {
		return nil, err
	}
	defs, err := publicDefaultPrivileges(ctx, in.Q)
	if err != nil {
		return nil, err
	}
	return append(out, defs...), nil
}

// schemaGrants collects one schema's PUBLIC relation grants.
type schemaGrants struct {
	schema string
	fix    []string
	shown  []string
	ev     []Evidence
}

func publicTableGrants(ctx context.Context, q Querier) ([]Finding, error) {
	rows, err := q.Query(ctx, ap15TablesSQL, apbMaxRows)
	if err != nil {
		return nil, fmt.Errorf("read PUBLIC table grants: %w", err)
	}
	defer rows.Close()
	var all []*schemaGrants
	for rows.Next() {
		var schema, rel string
		var privs []string
		if err := rows.Scan(&schema, &rel, &privs); err != nil {
			return nil, fmt.Errorf("read PUBLIC table grants: %w", err)
		}
		if len(all) == 0 || all[len(all)-1].schema != schema {
			all = append(all, &schemaGrants{schema: schema})
		}
		g := all[len(all)-1]
		name := QualifiedName(schema, rel)
		g.fix = append(g.fix, "REVOKE ALL ON TABLE "+name+" FROM PUBLIC;")
		g.shown = append(g.shown, rel+" ("+strings.Join(privs, ", ")+")")
		g.ev = append(g.ev, Evidence{Source: "pg_class.relacl", Ref: name,
			Detail: "PUBLIC: " + strings.Join(privs, ", ")})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read PUBLIC table grants: %w", err)
	}
	out := make([]Finding, len(all))
	for i, g := range all {
		out[i] = Finding{Severity: Warning, ObjectType: "schema", Object: g.schema,
			Title: "Relations granted to PUBLIC",
			Detail: fmt.Sprintf("%d relations in %s are granted to PUBLIC, so every role, "+
				"every agent role included, holds these privileges: %s.", len(g.fix),
				g.schema, listSome(g.shown, 10)),
			Recommendation: "Revoke the PUBLIC grants and grant each relation to the roles " +
				"that need it.",
			FixScript: strings.Join(g.fix, "\n"), Evidence: g.ev}
	}
	return out, nil
}

var defaultObjects = map[string]string{"r": "tables", "S": "sequences"}

func publicDefaultPrivileges(ctx context.Context, q Querier) ([]Finding, error) {
	rows, err := q.Query(ctx, ap15DefaultsSQL, apbMaxRows)
	if err != nil {
		return nil, fmt.Errorf("read PUBLIC default privileges: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var owner, schema, kind string
		var privs []string
		if err := rows.Scan(&owner, &schema, &kind, &privs); err != nil {
			return nil, fmt.Errorf("read PUBLIC default privileges: %w", err)
		}
		out = append(out, defaultPrivilegeFinding(owner, schema, defaultObjects[kind], privs))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read PUBLIC default privileges: %w", err)
	}
	return out, nil
}

func defaultPrivilegeFinding(owner, schema, objects string, privs []string) Finding {
	object, scope, in := owner+": "+objects, "anywhere", ""
	if schema != "" {
		object = owner + " in " + schema + ": " + objects
		scope = "in schema " + schema
		in = " IN SCHEMA " + QuoteIdent(schema)
	}
	return Finding{Severity: Warning, ObjectType: "default_privileges", Object: object,
		Title: "Default privileges grant PUBLIC",
		Detail: fmt.Sprintf("New %s that %s creates %s are granted %s to PUBLIC, so every "+
			"agent role gets them.", objects, owner, scope, strings.Join(privs, ", ")),
		Recommendation: "Remove PUBLIC from the default privileges; grant new objects " +
			"to the roles that need them.",
		FixScript: "ALTER DEFAULT PRIVILEGES FOR ROLE " + QuoteIdent(owner) + in +
			" REVOKE ALL ON " + strings.ToUpper(objects) + " FROM PUBLIC;",
		Evidence: []Evidence{{Source: "pg_default_acl", Ref: object,
			Detail: "PUBLIC: " + strings.Join(privs, ", ")}}}
}
