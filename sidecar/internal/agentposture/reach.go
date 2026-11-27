package agentposture

import (
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// ReachOIDs lists the exposed role OIDs (PUBLIC first) and then the group
// roles they inherit, in OID order: every grantee whose privileges an
// exposed role holds.
func (e Env) ReachOIDs() []uint32 {
	out := e.ExposedOIDs()
	groups := make([]uint32, 0, len(e.Via))
	for g := range e.Via {
		if !slices.Contains(out, g) {
			groups = append(groups, g)
		}
	}
	slices.Sort(groups)
	return append(out, groups...)
}

// holds reports whether exposed role r holds what is granted to grantees:
// directly, through PUBLIC or through a group it inherits.
func (e Env) holds(r Role, grantees []uint32) bool {
	if slices.Contains(grantees, r.OID) || slices.Contains(grantees, PublicOID) {
		return true
	}
	return len(e.paths([]Role{r}, grantees)) > 0
}

// inheritedGroups lists the group roles the exposed roles inherit that are
// not exposed roles themselves, in OID order.
func (e Env) inheritedGroups() []Role {
	var out []Role
	for _, oid := range e.ReachOIDs()[len(e.Exposed):] {
		if g, ok := e.group(oid); ok {
			out = append(out, g)
		}
	}
	return out
}

// revokeAllTargets are the roles other than PUBLIC a blanket REVOKE must
// name so that no exposed role keeps a privilege: the exposed roles, then
// the group roles they inherit.
func (e Env) revokeAllTargets() []Role {
	var out []Role
	for _, r := range e.Exposed {
		if r.OID != PublicOID {
			out = append(out, r)
		}
	}
	return append(out, e.inheritedGroups()...)
}

// inheritedCaveat is the group caveat when a blanket REVOKE names a group.
func (e Env) inheritedCaveat() string {
	return groupCaveat(e.paths(e.Exposed, oids(e.inheritedGroups())))
}

// paths lists how the roles of rs inherit the groups among grantees.
func (e Env) paths(rs []Role, grantees []uint32) []Path {
	var out []Path
	for _, g := range grantees {
		for _, p := range e.Via[g] {
			if slices.ContainsFunc(rs, func(r Role) bool { return r.OID == p.Exposed }) &&
				!slices.ContainsFunc(out, func(x Path) bool { return x.String() == p.String() }) {
				out = append(out, p)
			}
		}
	}
	return out
}

// group returns group role oid as a Role when an exposed role inherits it.
func (e Env) group(oid uint32) (Role, bool) {
	ps := e.Via[oid]
	if len(ps) == 0 {
		return Role{}, false
	}
	return Role{OID: oid, Name: ps[0].Names[len(ps[0].Names)-1], Source: SourceGroup}, true
}

// roleName is the name of an exposed role or inherited group, "" otherwise.
func (e Env) roleName(oid uint32) string {
	if r, ok := e.Exposure(oid); ok {
		return r.Name
	}
	if g, ok := e.group(oid); ok {
		return g.Name
	}
	return ""
}

// noteVia reports a cut membership read as the detector's truncation: a
// grant to a group not read may reach an exposed role, so the detector's
// open findings stay open.
func (in Input) noteVia() {
	for _, tr := range in.Env.ViaTruncated {
		in.Truncated(tr.Arm, tr.Limit)
	}
}

// viaText is " (through a → g; b → g)" for paths, "" without any.
func viaText(ps []Path) string {
	if len(ps) == 0 {
		return ""
	}
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.String()
	}
	return " (through " + strings.Join(parts, "; ") + ")"
}

// viaEvidence is the membership evidence of ref for paths.
func viaEvidence(ref string, ps []Path) []Evidence {
	var out []Evidence
	for _, p := range ps {
		out = append(out, Evidence{Source: "pg_auth_members", Ref: ref,
			Detail: "inherited through " + p.String()})
	}
	return out
}

// membershipFix offers, for each path, ending the exposed role's first
// membership on it instead of revoking from the group. Via keeps only the
// shortest path, so the line says other memberships leading to the group
// must end too. On PostgreSQL 16+ a membership another role granted needs
// that grantor named.
func membershipFix(ps []Path) []string {
	var out []string
	for _, p := range ps {
		line := "-- or keep the group's grant and end the inheritance (and every other " +
			"membership leading to " + sqlident.Ident(p.Names[len(p.Names)-1]) + "; " +
			"PostgreSQL 16+: add GRANTED BY <grantor> when another role granted it): " +
			"REVOKE " + sqlident.Ident(p.Names[1]) + " FROM " + sqlident.Ident(p.Names[0]) + ";"
		if !slices.Contains(out, line) {
			out = append(out, line)
		}
	}
	return out
}

// groupCaveat warns that a REVOKE from a group reaches all its members.
func groupCaveat(ps []Path) string {
	if len(ps) == 0 {
		return ""
	}
	return "Revoking from a group role removes the privilege from every member of " +
		"that group, not only from the exposed roles; ending the membership instead " +
		"removes everything the group gives them."
}

// joinCaveats joins the non-empty caveats.
func joinCaveats(cs ...string) string {
	return strings.Join(slices.DeleteFunc(cs, func(c string) bool { return c == "" }), " ")
}
