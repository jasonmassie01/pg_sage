//go:build cgo

package sqlast

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// checkRoleSetting allows ALTER ROLE <one role> IN DATABASE <database>
// SET/RESET of a workload budget setting (market-08): never ALL, never
// every database.
func checkRoleSetting(stmt *pg_query.AlterRoleSetStmt, rules Rules) error {
	if rules.RoleParam == nil {
		return reject("ALTER ROLE ... SET is not allowed")
	}
	if stmt.GetRole().GetRoletype() != pg_query.RoleSpecType_ROLESPEC_CSTRING {
		return reject("ALTER ROLE must name one role")
	}
	if stmt.GetDatabase() == "" {
		return reject("ALTER ROLE ... SET must name the database (IN DATABASE)")
	}
	return checkSetting("ALTER ROLE", stmt.GetSetstmt(), rules.RoleParam)
}

// checkRoleConnectionLimit allows ALTER ROLE <one allowed role>
// CONNECTION LIMIT n and nothing else.
func checkRoleConnectionLimit(stmt *pg_query.AlterRoleStmt, rules Rules) error {
	if rules.RoleConnectionLimit == nil {
		return reject("ALTER ROLE ... CONNECTION LIMIT is not allowed")
	}
	if stmt.GetRole().GetRoletype() != pg_query.RoleSpecType_ROLESPEC_CSTRING {
		return reject("ALTER ROLE must name one role")
	}
	options := stmt.GetOptions()
	if len(options) != 1 || options[0].GetDefElem().GetDefname() != "connectionlimit" {
		return reject("ALTER ROLE may change only the connection limit")
	}
	if role := stmt.GetRole().GetRolename(); !rules.RoleConnectionLimit(role) {
		return reject("ALTER ROLE %q CONNECTION LIMIT is not allowed for this role", role)
	}
	return nil
}
