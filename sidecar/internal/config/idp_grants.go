package config

import (
	"fmt"
	"strings"
)

// OAuthDatabaseGrant maps one IdP group to a per-database grant
// (apiauth-08d). At every SSO login the grants a user's groups give
// replace the grants the IdP gave before; an admin's manual grants are
// never touched and win on the same database.
type OAuthDatabaseGrant struct {
	Group    string `yaml:"group" doc:"IdP group as the groups claim carries it (case-sensitive)."`
	Database string `yaml:"database" doc:"Database name as the fleet names it."`
	Role     string `yaml:"role" doc:"Grant for group members on the database: viewer or operator."`
}

// validateDatabaseGrantMapping checks oauth.database_grant_mapping at
// load: every entry names a group, one database and a grant role, and no
// group appears twice for the same database. Errors name the entry.
func (o *OAuthConfig) validateDatabaseGrantMapping() error {
	first := make(map[[2]string]int, len(o.DatabaseGrantMapping))
	for i, m := range o.DatabaseGrantMapping {
		entry := fmt.Sprintf("oauth.database_grant_mapping[%d] (group %q, database %q)",
			i, m.Group, m.Database)
		if strings.TrimSpace(m.Group) == "" {
			return fmt.Errorf("%s: group must not be empty", entry)
		}
		if !validGrantMappingDatabase(m.Database) {
			return fmt.Errorf("%s: database must name one database (not empty, all, * "+
				"or padded with spaces)", entry)
		}
		if m.Role != "viewer" && m.Role != "operator" {
			return fmt.Errorf("%s: role %q must be viewer or operator (admins are never "+
				"limited by database)", entry, m.Role)
		}
		key := [2]string{m.Group, m.Database}
		if j, dup := first[key]; dup {
			return fmt.Errorf("%s repeats group %q on database %q (first at [%d])",
				entry, m.Group, m.Database, j)
		}
		first[key] = i
	}
	return nil
}

func validGrantMappingDatabase(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && name != "all" && name != "*"
}
