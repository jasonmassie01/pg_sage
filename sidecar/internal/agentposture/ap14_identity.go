package agentposture

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// roleLabel names which of pg_sage's roles a finding is about.
func roleLabel(r Role) string {
	if r.Source == SourceAction {
		return "pg_sage's action role"
	}
	return "pg_sage's role"
}

// actingWhy lists the acting rights a role inherits and how to revoke them.
func actingWhy(r *roleRead) (why, fix []string) {
	who := sqlident.Ident(r.role.Name)
	if len(r.owners) > 0 {
		why = append(why, "inherits the table owner roles "+strings.Join(r.owners, ", "))
		for _, o := range r.owners {
			fix = append(fix, "REVOKE "+sqlident.Ident(o)+" FROM "+who+";")
		}
	}
	if r.signal {
		why = append(why, "inherits pg_signal_backend")
		fix = append(fix, "REVOKE pg_signal_backend FROM "+who+";")
	}
	return why, fix
}

// observationFindings checks the observation role of a split identity:
// it must neither act (owner membership, pg_signal_backend) nor bypass
// RLS or inherit agent roles. One finding names every reason.
func observationFindings(in Input, self *roleRead) []Finding {
	if self.super {
		return []Finding{superuserSelf(self.role)}
	}
	why, fix := actingWhy(self)
	if self.bypassOrInherits {
		f := selfFinding(in, self.role, self.bypass, self.inherited)
		if len(why) == 0 {
			return []Finding{f}
		}
		f.Detail += " It also " + strings.Join(why, " and ") + "."
		f.FixScript += "\n" + strings.Join(fix, "\n")
		return []Finding{f}
	}
	if len(why) == 0 {
		return nil
	}
	who := self.role.Name
	return []Finding{{Severity: Warning, ObjectType: "role", Object: who,
		Title: "pg_sage's observation role holds acting rights",
		Detail: "pg_sage's observation role " + who + " " + strings.Join(why, " and ") +
			", although a separate action identity (action_dsn) acts: a probe bug or " +
			"injection on the observation pool would then run with owner rights or " +
			"end other sessions.",
		Recommendation: "Keep table owner memberships and pg_signal_backend on the action " +
			"role only; the observation role needs pg_monitor and the sage schema.",
		FixScript: strings.Join(fix, "\n"),
		Caveat:    "A membership inherited through another role is undone on that role's grant.",
		Evidence: []Evidence{{Source: "pg_roles", Ref: who,
			Detail: strings.Join(why, "; ")}}}}
}

// oneIdentityFinding notes that one role observes and acts: supported,
// but every probe then runs with the rights the actions need.
func oneIdentityFinding(self *roleRead) Finding {
	why, revoke := actingWhy(self)
	who := self.role.Name
	grant := []string{"CREATE ROLE sage_action LOGIN; -- then set its password",
		"GRANT pg_signal_backend, pg_read_all_stats TO sage_action;"}
	for _, o := range self.owners {
		grant = append(grant, "GRANT "+sqlident.Ident(o)+" TO sage_action;")
	}
	grant = append(grant, "-- point action_dsn at sage_action, restart, then:")
	return Finding{Severity: Info, ObjectType: "role", Object: who,
		Title: "pg_sage runs as one identity that observes and acts",
		Detail: "pg_sage's role " + who + " " + strings.Join(why, " and ") +
			", and the same role runs every probe: a probe bug or injection would run " +
			"with owner rights and could end other sessions.",
		Recommendation: "Set action_dsn to a second role that holds the owner memberships " +
			"and pg_signal_backend, and keep this role to pg_monitor and the sage schema " +
			"(Getting started > Grant more prints both scripts).",
		FixScript: strings.Join(append(grant, revoke...), "\n"),
		Evidence: []Evidence{{Source: "pg_roles", Ref: who,
			Detail: strings.Join(why, "; ")}}}
}
