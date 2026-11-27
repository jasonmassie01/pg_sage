package auth

// VisibleDatabases is the read side of the grant rules (apiauth-08b): the
// databases a user may read. An admin, and a user with no grants, is
// unscoped (nil, false): no filter. A user with any grant sees exactly the
// granted databases, whatever the grant's role, as a non-nil set. An
// unknown global role fails closed: scoped to nothing. Names a grant can
// never hold ("", "all", "*") are ignored, never read as "every database".
func VisibleDatabases(globalRole string, grants map[string]string) (map[string]bool, bool) {
	if roleRanks[globalRole] == 0 {
		return map[string]bool{}, true
	}
	if globalRole == RoleAdmin || len(grants) == 0 {
		return nil, false
	}
	set := make(map[string]bool, len(grants))
	for db := range grants {
		if validGrantDatabase(db) {
			set[db] = true
		}
	}
	return set, true
}
