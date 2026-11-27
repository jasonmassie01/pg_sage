package agentposture

import (
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// Shared parts of the AP-09..AP-16 statements and fix scripts.

// apbUserSchemas keeps user schemas: no system schema and not pg_sage's.
const apbUserSchemas = `n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
  AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp'`

// apbMaxRows bounds each AP-09..AP-16 catalog read; a variable so tests
// can reach the limit with a few rows.
var apbMaxRows = 5000

// exposedHolders lists the exposed roles that hold a privilege granted to
// grantees: directly, through PUBLIC or through a group role they inherit.
func exposedHolders(env Env, grantees []uint32) []Role {
	var out []Role
	for _, e := range env.Exposed {
		if env.holds(e, grantees) {
			out = append(out, e)
		}
	}
	return out
}

// reaching narrows holders to those that also hold USAGE on the object's
// schema (usage lists its grantees), in any of the ways exposedHolders
// counts.
func reaching(env Env, holders []Role, usage []uint32) []Role {
	return slices.DeleteFunc(slices.Clone(holders), func(r Role) bool {
		return !env.holds(r, usage)
	})
}

// revokeTargets are the grantees a REVOKE must name so that none of rs
// keeps the privilege: PUBLIC when PUBLIC holds it, each role of rs
// granted it directly, then each granted group role a role of rs
// inherits (in the order of grantees).
func revokeTargets(env Env, rs []Role, grantees []uint32) []Role {
	var out []Role
	if slices.Contains(grantees, PublicOID) {
		out = append(out, Role{OID: PublicOID, Name: "PUBLIC", Source: SourcePublic})
	}
	for _, r := range rs {
		if r.OID != PublicOID && slices.Contains(grantees, r.OID) {
			out = mergeRoles(out, []Role{r})
		}
	}
	for _, g := range grantees {
		if len(env.paths(rs, []uint32{g})) == 0 {
			continue
		}
		if r, ok := env.group(g); ok {
			out = mergeRoles(out, []Role{r})
		}
	}
	return out
}

// revokeTarget is a role as a GRANT/REVOKE target.
func revokeTarget(r Role) string {
	if r.OID == PublicOID {
		return "PUBLIC"
	}
	return sqlident.Ident(r.Name)
}

// roleText is "a, b and c" of the role names.
func roleText(rs []Role) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	switch len(out) {
	case 0:
		return ""
	case 1:
		return out[0]
	}
	return strings.Join(out[:len(out)-1], ", ") + " and " + out[len(out)-1]
}
