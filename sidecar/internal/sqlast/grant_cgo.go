//go:build cgo

package sqlast

import pg_query "github.com/pganalyze/pg_query_go/v6"

// publicTablePrivileges are the privileges an exposure GRANT or REVOKE may
// name (fleetpol-02b).
var publicTablePrivileges = map[string]bool{"select": true, "insert": true,
	"update": true, "delete": true, "truncate": true, "references": true, "trigger": true}

// checkPublicGrant accepts only the exposure forms: REVOKE of table
// privileges on one schema-qualified table FROM PUBLIC, and its re-GRANT TO
// PUBLIC. No column list, grant option, GRANTED BY or CASCADE.
func checkPublicGrant(g *pg_query.GrantStmt) error {
	if g.GetTargtype() != pg_query.GrantTargetType_ACL_TARGET_OBJECT ||
		g.GetObjtype() != pg_query.ObjectType_OBJECT_TABLE || len(g.GetObjects()) != 1 {
		return reject("GRANT/REVOKE must name exactly one table")
	}
	if rv := g.GetObjects()[0].GetRangeVar(); rv == nil || rv.GetSchemaname() == "" {
		return reject("GRANT/REVOKE target must be schema-qualified")
	}
	if g.GetGrantOption() || g.GetGrantor() != nil ||
		g.GetBehavior() == pg_query.DropBehavior_DROP_CASCADE {
		return reject("GRANT/REVOKE options are not allowed")
	}
	if len(g.GetGrantees()) != 1 ||
		g.GetGrantees()[0].GetRoleSpec().GetRoletype() != pg_query.RoleSpecType_ROLESPEC_PUBLIC {
		return reject("GRANT/REVOKE is allowed only to or from PUBLIC")
	}
	for _, p := range g.GetPrivileges() {
		priv := p.GetAccessPriv()
		if priv == nil || len(priv.GetCols()) > 0 || !publicTablePrivileges[priv.GetPrivName()] {
			return reject("GRANT/REVOKE privilege is not a whole-table privilege")
		}
	}
	return nil
}
