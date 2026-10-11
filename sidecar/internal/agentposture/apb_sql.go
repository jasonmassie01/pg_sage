package agentposture

import (
	"slices"
	"strings"
)

// Shared parts of the AP-09..AP-16 statements and fix scripts.

// apbUserSchemas keeps user schemas: no system schema and not pg_sage's.
const apbUserSchemas = `n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
  AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp'`

// apbMaxRows bounds each AP-09..AP-16 catalog read.
const apbMaxRows = 5000

// exposedHolders lists the exposed roles that hold a privilege granted to
// grantees, directly or through PUBLIC.
func exposedHolders(env Env, grantees []uint32) []Role {
	var out []Role
	for _, e := range env.Exposed {
		if slices.Contains(grantees, e.OID) || slices.Contains(grantees, PublicOID) {
			out = append(out, e)
		}
	}
	return out
}

// reaching narrows holders to those that also hold USAGE on the object's
// schema (usage lists its grantees), directly or through PUBLIC.
func reaching(holders []Role, usage []uint32) []Role {
	return slices.DeleteFunc(slices.Clone(holders), func(r Role) bool {
		return !slices.Contains(usage, r.OID) && !slices.Contains(usage, PublicOID)
	})
}

// revokeTargets are the grantees a REVOKE must name so that none of rs
// keeps the privilege: each role of rs granted it directly, and PUBLIC
// when PUBLIC holds it.
func revokeTargets(rs []Role, grantees []uint32) []Role {
	var out []Role
	if slices.Contains(grantees, PublicOID) {
		out = append(out, Role{OID: PublicOID, Name: "PUBLIC", Source: SourcePublic})
	}
	for _, r := range rs {
		if r.OID != PublicOID && slices.Contains(grantees, r.OID) {
			out = append(out, r)
		}
	}
	return out
}

// revokeTarget is a role as a GRANT/REVOKE target.
func revokeTarget(r Role) string {
	if r.OID == PublicOID {
		return "PUBLIC"
	}
	return QuoteIdent(r.Name)
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
