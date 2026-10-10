package agentposture

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

func init() { Register(ap14{}) }

// ap14 reports pg_sage's own role when it bypasses row-level security
// (BYPASSRLS, or superuser) or holds the privileges of agent roles: a
// bypass of pg_sage would then reach every row and everything the agents
// can do (spec R1). Warning. Inheritance is read with pg_has_role on
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

var ap14SQL = Statement("AP-14", `SELECT r.rolsuper, r.rolbypassrls,
  ARRAY(SELECT g.rolname::text FROM pg_catalog.pg_roles g
        WHERE g.oid = ANY($2::oid[]) AND pg_catalog.pg_has_role($1::oid, g.oid, 'USAGE')
        ORDER BY 1)
FROM pg_catalog.pg_roles r
WHERE r.oid = $1::oid`)

func (ap14) Detect(ctx context.Context, in Input) ([]Finding, error) {
	self := in.Env.Self
	var super, bypass bool
	var inherited []string
	err := in.Q.QueryRow(ctx, ap14SQL, self.OID, in.Env.AgentOIDs()).Scan(&super, &bypass,
		&inherited)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pg_sage's own role: %w", err)
	}
	if super {
		return []Finding{superuserSelf(self)}, nil
	}
	if !bypass && len(inherited) == 0 {
		return nil, nil
	}
	return []Finding{selfFinding(in, self, bypass, inherited)}, nil
}

func superuserSelf(self Role) Finding {
	return Finding{Severity: Warning, ObjectType: "role", Object: self.Name,
		Title: "pg_sage connects as a superuser",
		Detail: "pg_sage's role " + self.Name + " is a superuser: it bypasses row-level " +
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
	who := QuoteIdent(self.Name)
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
		Detail: "pg_sage's role " + self.Name + " " + strings.Join(why, " and ") +
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
			fix = append(fix, "REVOKE INHERIT OPTION FOR "+QuoteIdent(a)+" FROM "+who+";")
		} else {
			fix = append(fix, "REVOKE "+QuoteIdent(a)+" FROM "+who+";")
		}
	}
	if in.Arm("inherit_role_level") {
		fix = append(fix, "-- or keep the memberships without inheriting them: ALTER ROLE "+
			who+" NOINHERIT;")
	}
	return fix
}
