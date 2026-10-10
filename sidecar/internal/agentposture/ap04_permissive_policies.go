package agentposture

import (
	"context"
	"fmt"
	"strings"
)

func init() { Register(ap04{}) }

// ap04 reports permissive row-level security policies for exposed roles
// (or PUBLIC) whose USING or WITH CHECK expression is plain true: the
// policy lets those roles read or write every row it covers.
type ap04 struct{}

func (ap04) Spec() Spec {
	return Spec{ID: "AP-04", Title: "Permissive always-true policies for exposed roles",
		Severity: Warning}
}

var ap04SQL = Statement("AP-04", `SELECT n.nspname::text, c.relname::text,
  p.polname::text, p.polcmd::text, p.polroles::oid[], c.relrowsecurity,
  COALESCE(pg_catalog.pg_get_expr(p.polqual, p.polrelid), ''),
  COALESCE(pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid), '')
FROM pg_catalog.pg_policy p
JOIN pg_catalog.pg_class c ON c.oid = p.polrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE p.polpermissive AND p.polroles::oid[] && $1::oid[] AND `+userSchemaFilter+`
  AND (pg_catalog.pg_get_expr(p.polqual, p.polrelid) = 'true'
    OR pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid) = 'true')
ORDER BY n.nspname, c.relname, p.polname
LIMIT $2`)

var policyCommands = map[string]string{"r": "SELECT", "a": "INSERT", "w": "UPDATE",
	"d": "DELETE", "*": "ALL"}

type ap04Row struct {
	schema, table, policy, cmd string
	roles                      []uint32
	rls                        bool
	qual, check                string
}

func (ap04) Detect(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, ap04SQL, in.Env.ExposedOIDs(), maxRows)
	if err != nil {
		return nil, fmt.Errorf("read permissive policies: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var r ap04Row
		if err := rows.Scan(&r.schema, &r.table, &r.policy, &r.cmd, &r.roles, &r.rls,
			&r.qual, &r.check); err != nil {
			return nil, fmt.Errorf("read permissive policies: %w", err)
		}
		out = append(out, ap04Finding(in.Env, r))
	}
	return out, rows.Err()
}

func ap04Finding(env Env, r ap04Row) Finding {
	var who []Role
	for _, oid := range r.roles {
		if e, ok := env.Exposure(oid); ok {
			who = append(who, e)
		}
	}
	var exprs []string
	if r.qual == "true" {
		exprs = append(exprs, "USING (true)")
	}
	if r.check == "true" {
		exprs = append(exprs, "WITH CHECK (true)")
	}
	table := r.schema + "." + r.table
	qt := QualifiedName(r.schema, r.table)
	cmd := policyCommands[r.cmd]
	detail := fmt.Sprintf("Permissive %s policy %s on %s applies to %s with %s, so it "+
		"allows every row.", cmd, r.policy, table, roleList(who), strings.Join(exprs, " and "))
	if !r.rls {
		detail += " Row-level security is disabled on the table, so the policy takes " +
			"effect once it is enabled."
	}
	clause := "USING"
	if r.qual != "true" {
		clause = "WITH CHECK"
	}
	return Finding{Severity: Warning, ObjectType: "policy", Object: table + ":" + r.policy,
		Title: fmt.Sprintf("Policy %s on %s allows every row to %s", r.policy, table,
			roleList(who)),
		Detail: detail,
		Recommendation: "Restrict the policy to the rows each user owns, or drop it if " +
			"the table is not meant to be open.",
		FixScript: fmt.Sprintf("-- Replace the predicate with the rows each user may "+
			"reach.\nALTER POLICY %s ON %s %s (owner_id = current_user);\n-- or:\n"+
			"-- DROP POLICY %s ON %s;", QuoteIdent(r.policy), qt, clause,
			QuoteIdent(r.policy), qt),
		Caveat: "Check what the application expects before narrowing a policy.",
		Evidence: []Evidence{{Source: "pg_policy", Ref: table + ":" + r.policy,
			Detail: cmd + " " + strings.Join(exprs, " ")}}}
}
