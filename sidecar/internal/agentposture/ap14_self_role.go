package agentposture

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

func init() { Register(ap14{}) }

// ap14 reports pg_sage's own role when it bypasses row-level security
// (BYPASSRLS, or superuser) or holds the privileges of agent roles: a
// bypass of pg_sage would then reach every row and everything the agents
// can do (spec R1). Warning. With a separate action identity (action_dsn,
// security-09) it checks both roles and warns when the observation role
// holds acting rights (table-owner membership, pg_signal_backend); one role
// that acts is reported as info. Inheritance is read with pg_has_role on
// every version; the arms differ in how it is undone: PG14/15 inherit per
// role (rolinherit), PG16+ per grant (INHERIT option).
type ap14 struct{}

func (ap14) Spec() Spec {
	return Spec{ID: "AP-14", Title: "pg_sage's own role bypasses RLS or inherits agent roles",
		Severity: Warning, Arms: []Arm{
			{Name: "inherit_role_level", MaxVersion: 160000,
				SkipReason: "PostgreSQL 16+ grants INHERIT per membership; the per-grant arm " +
					"applies"},
			{Name: "inherit_per_grant", MinVersion: 160000,
				SkipReason: "before PostgreSQL 16, inheritance is the member role's " +
					"rolinherit attribute; the role-level arm applies"},
		}}
}

// ap14SQL reads one of pg_sage's roles: its attributes, the agent roles it
// inherits, the owners of user tables it inherits (sage and the system
// schemas left out; at most ap14OwnerLimit) and pg_signal_backend.
var ap14SQL = Statement("AP-14", `SELECT r.rolsuper, r.rolbypassrls,
  ARRAY(SELECT g.rolname::text FROM pg_catalog.pg_roles g
        WHERE g.oid = ANY($2::oid[]) AND pg_catalog.pg_has_role($1::oid, g.oid, 'USAGE')
        ORDER BY 1),
  ARRAY(SELECT o.rolname::text FROM pg_catalog.pg_roles o
        WHERE o.oid IN (SELECT c.relowner FROM pg_catalog.pg_class c
                        JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
                        WHERE c.relkind IN ('r', 'p', 'm')
                          AND n.nspname NOT IN ('sage', 'information_schema')
                          AND n.nspname !~ '^pg_')
          AND pg_catalog.pg_has_role($1::oid, o.oid, 'USAGE')
        ORDER BY 1 LIMIT $3),
  pg_catalog.pg_has_role($1::oid, 'pg_signal_backend', 'USAGE')
FROM pg_catalog.pg_roles r
WHERE r.oid = $1::oid`)

// ap14OwnerLimit is how many inherited table owners a finding names.
const ap14OwnerLimit = 10

// roleRead is what ap14SQL read about one role.
type roleRead struct {
	role             Role
	super, bypass    bool
	inherited        []string // agent roles it inherits
	owners           []string // table owners it inherits
	signal           bool     // inherits pg_signal_backend
	actingRights     bool     // owners or signal
	bypassOrInherits bool
}

func readRole(ctx context.Context, in Input, role Role) (*roleRead, error) {
	r := roleRead{role: role}
	err := in.Q.QueryRow(ctx, ap14SQL, role.OID, in.Env.AgentOIDs(), ap14OwnerLimit).
		Scan(&r.super, &r.bypass, &r.inherited, &r.owners, &r.signal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pg_sage's role %s: %w", role.Name, err)
	}
	r.actingRights = len(r.owners) > 0 || r.signal
	r.bypassOrInherits = r.bypass || len(r.inherited) > 0
	return &r, nil
}

// Detect checks pg_sage's role, and with a separate action identity both:
// the observation role must not hold acting rights, and neither may bypass
// RLS or inherit agent roles. One role that acts is the supported single
// identity: reported as info.
func (ap14) Detect(ctx context.Context, in Input) ([]Finding, error) {
	self, err := readRole(ctx, in, in.Env.Self)
	if err != nil || self == nil {
		return nil, err
	}
	if in.Env.Action == nil {
		return singleIdentityFindings(in, self), nil
	}
	out := observationFindings(in, self)
	act, err := readRole(ctx, in, *in.Env.Action)
	if err != nil || act == nil {
		return out, err
	}
	switch {
	case act.super:
		out = append(out, superuserSelf(act.role))
	case act.bypassOrInherits:
		out = append(out, selfFinding(in, act.role, act.bypass, act.inherited))
	}
	return out, nil
}

func singleIdentityFindings(in Input, self *roleRead) []Finding {
	switch {
	case self.super:
		return []Finding{superuserSelf(self.role)}
	case self.bypassOrInherits:
		return []Finding{selfFinding(in, self.role, self.bypass, self.inherited)}
	case self.actingRights:
		return []Finding{oneIdentityFinding(self)}
	}
	return nil
}

func superuserSelf(self Role) Finding {
	return Finding{Severity: Warning, ObjectType: "role", Object: self.Name,
		Title: "pg_sage connects as a superuser",
		Detail: roleLabel(self) + " " + self.Name + " is a superuser: it bypasses row-level " +
			"security and holds every role's privileges, agent roles included. Agent " +
			"Guard's role management will not run as a superuser.",
		Recommendation: "Connect pg_sage as a dedicated role without SUPERUSER or " +
			"BYPASSRLS, with the grants Getting started lists.",
		FixScript: "CREATE ROLE pg_sage_agent LOGIN NOSUPERUSER NOBYPASSRLS IN ROLE " +
			"pg_monitor;\n-- grant what Getting started > Grant more lists, then point " +
			"pg_sage's connection at pg_sage_agent",
		Evidence: []Evidence{{Source: "pg_roles", Ref: self.Name, Detail: "rolsuper = true"}}}
}

func selfFinding(in Input, self Role, bypass bool, inherited []string) Finding {
	var why, fix []string
	who := sqlident.Ident(self.Name)
	if bypass {
		why = append(why, "holds BYPASSRLS")
		fix = append(fix, "ALTER ROLE "+who+" NOBYPASSRLS;")
	}
	if len(inherited) > 0 {
		why = append(why, "inherits agent roles "+strings.Join(inherited, ", "))
		fix = append(fix, inheritFix(in, who, inherited)...)
	}
	return Finding{Severity: Warning, ObjectType: "role", Object: self.Name,
		Title: "pg_sage's own role bypasses RLS or inherits agent roles",
		Detail: roleLabel(self) + " " + self.Name + " " + strings.Join(why, " and ") +
			": a bypass of pg_sage reaches what row-level security or the agents' " +
			"boundaries should stop.",
		Recommendation: "Remove BYPASSRLS from pg_sage's role and hold agent roles " +
			"without inheriting them (PostgreSQL 16+: WITH INHERIT FALSE).",
		FixScript: strings.Join(fix, "\n"),
		Caveat: "A membership inherited through another role is undone on that " +
			"role's grant, not on pg_sage's.",
		Evidence: []Evidence{{Source: "pg_roles", Ref: self.Name,
			Detail: strings.Join(why, "; ")}}}
}

// inheritFix undoes the inheritance by the version's arm.
func inheritFix(in Input, who string, inherited []string) []string {
	var fix []string
	for _, a := range inherited {
		if in.Arm("inherit_per_grant") {
			fix = append(fix, "REVOKE INHERIT OPTION FOR "+sqlident.Ident(a)+" FROM "+who+";")
		} else {
			fix = append(fix, "REVOKE "+sqlident.Ident(a)+" FROM "+who+";")
		}
	}
	if in.Arm("inherit_role_level") {
		fix = append(fix, "-- or keep the memberships without inheriting them: ALTER ROLE "+
			who+" NOINHERIT;")
	}
	return fix
}
