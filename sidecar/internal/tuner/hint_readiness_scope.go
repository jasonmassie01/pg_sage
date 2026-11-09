package tuner

import "strings"

// scopeValues are the two settings one kind of session starts with.
type scopeValues struct {
	session string // session_preload_libraries
	tableOn string // pg_hint_plan.enable_hint_table
}

// apply evaluates the settings for the database's default sessions and for
// every login role with settings of its own. It fills SharedPreload,
// Preload and HintTableOn, adds the reasons, and reports whether one kind
// of session has both pg_hint_plan preloaded and the hint table on: a role
// that preloads the module and another that turns the table on give no
// session that applies hints.
func (s hintSettings) apply(out *HintPlanAvailability) bool {
	out.SharedPreload = s.shared != nil && hasHintLibrary(*s.shared)
	same := false
	for _, scope := range s.scopes() {
		preloaded := out.SharedPreload || hasHintLibrary(scope.session)
		on := settingIsOn(scope.tableOn)
		out.Preload = out.Preload || preloaded
		out.HintTableOn = out.HintTableOn || on
		same = same || (preloaded && on)
	}
	switch {
	case !out.Preload && s.shared == nil:
		out.Reasons = append(out.Reasons, reasonPreloadUnreadable)
	case !out.Preload:
		out.Reasons = append(out.Reasons, reasonNotPreloaded)
	}
	if !out.HintTableOn {
		out.Reasons = append(out.Reasons, reasonHintTableOff)
	}
	if out.Preload && out.HintTableOn && !same {
		out.Reasons = append(out.Reasons, reasonRoleMismatch)
	}
	return same
}

// scopes returns the effective settings of the database's default sessions
// and of each role with settings, in PostgreSQL's precedence: server, then
// ALTER ROLE ALL, then ALTER DATABASE, then ALTER ROLE, then ALTER ROLE IN
// DATABASE. The default scope assumes some application role sets neither.
func (s hintSettings) scopes() []scopeValues {
	base := scopeValues{session: s.session, tableOn: s.tableOn}
	base = s.overlay(base, "", false)
	base = s.overlay(base, "", true)
	out := []scopeValues{base}
	for _, role := range s.roleNames() {
		out = append(out, s.overlay(s.overlay(base, role, false), role, true))
	}
	return out
}

// overlay applies the pg_db_role_setting row for (role, inDatabase).
// Entries without "=" are ignored.
func (s hintSettings) overlay(v scopeValues, role string, inDatabase bool) scopeValues {
	for _, row := range s.roles {
		if row.Role != role || row.Database != inDatabase {
			continue
		}
		for _, entry := range row.Config {
			name, value, ok := strings.Cut(entry, "=")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "session_preload_libraries":
				v.session = value
			case "pg_hint_plan.enable_hint_table":
				v.tableOn = value
			}
		}
	}
	return v
}

// roleNames lists, once each, the roles with settings of their own.
func (s hintSettings) roleNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, row := range s.roles {
		if row.Role != "" && !seen[row.Role] {
			seen[row.Role] = true
			out = append(out, row.Role)
		}
	}
	return out
}
