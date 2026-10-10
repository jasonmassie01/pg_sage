package agentposture

import (
	"context"
	"fmt"
	"strings"
)

func init() { Register(ap01{}) }

// ap01 reports agent roles that are superuser or hold BYPASSRLS,
// CREATEROLE, CREATEDB or REPLICATION, or belong to a role that reads or
// writes server files, runs server programs or writes all data. Critical
// for a registered agent role, a warning for a client-name hint.
type ap01 struct{}

func (ap01) Spec() Spec {
	return Spec{ID: "AP-01", Title: "Agent roles with dangerous attributes",
		Severity: Critical}
}

// dangerousGroups are the predefined roles an agent role must not reach.
var dangerousGroups = []string{"pg_execute_server_program", "pg_write_server_files",
	"pg_read_server_files", "pg_write_all_data"}

var ap01SQL = Statement("AP-01", `SELECT r.oid, r.rolname::text, r.rolsuper,
  r.rolbypassrls, r.rolcreaterole, r.rolcreatedb, r.rolreplication,
  ARRAY(SELECT g.rolname::text FROM pg_catalog.pg_roles g
        WHERE g.rolname = ANY($2::text[]) AND pg_catalog.pg_has_role(r.oid, g.oid, 'MEMBER')
        ORDER BY g.rolname),
  ARRAY(SELECT g.rolname::text FROM pg_catalog.pg_auth_members m
        JOIN pg_catalog.pg_roles g ON g.oid = m.roleid
        WHERE m.member = r.oid AND g.rolname = ANY($2::text[]) ORDER BY g.rolname)
FROM pg_catalog.pg_roles r
WHERE r.oid = ANY($1::oid[])
ORDER BY r.rolname`)

type roleAttrs struct {
	oid                                 uint32
	name                                string
	super, bypass, createRole, createDB bool
	replication                         bool
	groups, direct                      []string
}

func (ap01) Detect(ctx context.Context, in Input) ([]Finding, error) {
	if len(in.Env.Agents) == 0 {
		return nil, nil
	}
	rows, err := in.Q.Query(ctx, ap01SQL, in.Env.AgentOIDs(), dangerousGroups)
	if err != nil {
		return nil, fmt.Errorf("read agent role attributes: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var a roleAttrs
		if err := rows.Scan(&a.oid, &a.name, &a.super, &a.bypass, &a.createRole,
			&a.createDB, &a.replication, &a.groups, &a.direct); err != nil {
			return nil, fmt.Errorf("read agent role attributes: %w", err)
		}
		role, _ := in.Env.Agent(a.oid)
		if f, ok := ap01Finding(role, a); ok {
			out = append(out, f)
		}
	}
	return out, rows.Err()
}

func ap01Finding(role Role, a roleAttrs) (Finding, bool) {
	attrs, clear := a.flagged()
	groups := a.groups
	if a.super {
		groups = nil // a superuser is a member of every role
	}
	if len(attrs) == 0 && len(groups) == 0 {
		return Finding{}, false
	}
	what := append(append([]string{}, attrs...), groups...)
	f := Finding{Severity: Critical, ObjectType: "role", Object: a.name,
		Title: fmt.Sprintf("Agent role %s holds %s", a.name, joinAnd(what)),
		Detail: fmt.Sprintf("Registered agent role %s is %s. An agent with these "+
			"rights can bypass row-level security, create roles or databases, or reach "+
			"the server's files and programs.", a.name, joinAnd(what)),
		Recommendation: "Remove the attributes and memberships; agent roles need only " +
			"the grants their task uses.",
		Evidence: []Evidence{{Source: "pg_roles", Ref: a.name, Detail: joinAnd(what)}}}
	if !role.Registered() {
		f.Severity = Warning
		f.Detail = fmt.Sprintf("Role %s connects with %q, which looks like an agent "+
			"client (a hint from agents.client_patterns, not a registered agent), and "+
			"is %s.", a.name, role.Hint, joinAnd(what))
	}
	f.FixScript = ap01Script(a, clear, groups)
	if len(groups) > len(a.direct) {
		f.Caveat = "Some memberships are inherited through another role; revoke them " +
			"where they are granted."
	}
	return f, true
}

// flagged lists the dangerous attributes and the clauses that clear them.
func (a roleAttrs) flagged() (attrs, clear []string) {
	for _, x := range []struct {
		on          bool
		name, unset string
	}{{a.super, "SUPERUSER", "NOSUPERUSER"}, {a.bypass, "BYPASSRLS", "NOBYPASSRLS"},
		{a.createRole, "CREATEROLE", "NOCREATEROLE"}, {a.createDB, "CREATEDB", "NOCREATEDB"},
		{a.replication, "REPLICATION", "NOREPLICATION"}} {
		if x.on {
			attrs = append(attrs, x.name)
			clear = append(clear, x.unset)
		}
	}
	return attrs, clear
}

func ap01Script(a roleAttrs, clear, groups []string) string {
	var b strings.Builder
	role := QuoteIdent(a.name)
	if len(clear) > 0 {
		fmt.Fprintf(&b, "ALTER ROLE %s %s;\n", role, strings.Join(clear, " "))
	}
	for _, g := range groups {
		if containsString(a.direct, g) {
			fmt.Fprintf(&b, "REVOKE %s FROM %s;\n", g, role)
		} else {
			fmt.Fprintf(&b, "-- %s reaches %s through another role: revoke it there.\n",
				role, g)
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
