package agentposture

import (
	"slices"
	"strings"
)

// userSchemaFilter keeps user schemas: no system schema and not pg_sage's.
const userSchemaFilter = `n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
  AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp'`

// schemaUsageGrantees is the array of roles holding USAGE on schema n
// (0 is PUBLIC), from the schema's ACL or its default.
const schemaUsageGrantees = `ARRAY(SELECT u.grantee FROM pg_catalog.aclexplode(
    COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) u
  WHERE u.privilege_type = 'USAGE')::oid[]`

// notExtensionMember excludes relation c when an extension owns it: its
// grants are the extension's, not the operator's.
const notExtensionMember = `NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend e
  WHERE e.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND e.objid = c.oid
    AND e.objsubid = 0 AND e.deptype = 'e')`

// maxRows bounds each detector's catalog read.
const maxRows = 50000

// reachingRoles lists the exposed roles that hold a privilege granted to
// grantees (directly or through PUBLIC) and USAGE on its schema (granted
// to them or to PUBLIC). Membership in other roles is not followed: the
// check reads ACLs as aclexplode reports them.
func reachingRoles(env Env, grantees, usage []uint32) []Role {
	var out []Role
	for _, e := range env.Exposed {
		holds := slices.Contains(grantees, e.OID) || slices.Contains(grantees, PublicOID)
		uses := slices.Contains(usage, e.OID) || slices.Contains(usage, PublicOID)
		if holds && uses {
			out = append(out, e)
		}
	}
	return out
}

// roleList is "a, b and c" of the role names, with how PUBLIC applies.
func roleList(rs []Role) string {
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.Name
	}
	return joinAnd(names)
}

func joinAnd(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// grantTarget is a role as a GRANT/REVOKE target.
func grantTarget(r Role) string {
	if r.OID == PublicOID {
		return "PUBLIC"
	}
	return QuoteIdent(r.Name)
}

func containsOID(xs []uint32, x uint32) bool { return slices.Contains(xs, x) }
