package upkeep

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// expected is the login and connection limit governance recorded for a
// role: a killed role cannot log in (limit 0); otherwise the broker login
// logs in within agents.broker.pool_max_conns and the direct-lane role is
// NOLOGIN in G1 (limit agents.roles.connection_limit).
type expected struct {
	login bool
	limit int
}

func expectedAttrs(cr agentguard.ClusterRole, role string,
	rc agentguard.RoleConfig) expected {
	switch {
	case cr.Status == agentguard.RoleStatusKilled:
		return expected{login: false, limit: 0}
	case role == cr.BrokerRole:
		return expected{login: true, limit: rc.BrokerConnectionLimit}
	default:
		return expected{login: false, limit: rc.ConnectionLimit}
	}
}

// attributeDrift compares a role's cluster-wide state with what governance
// recorded. It returns the drift with a Fix for what a person must change
// (superuser-only attributes, memberships, ownership), and the narrowing
// ALTER ROLE governance runs itself (login and connection limit: pg_sage
// holds ADMIN on the roles it created).
func attributeDrift(st agentguard.RoleState, exp expected) (RoleDrift, []string) {
	var d RoleDrift
	var fix []string
	role := ident(st.Name)
	if v := st.AgentViolations(); len(v) > 0 {
		d.Widening = append(d.Widening, v...)
		fix = append(fix, manualAttributeFixes(st, role)...)
	}
	for _, m := range st.MemberOf {
		if slices.Contains(st.Dangerous, m) {
			continue // AgentViolations names it
		}
		d.Widening = append(d.Widening, "is a member of "+m)
		fix = append(fix, fmt.Sprintf("REVOKE %s FROM %s;", ident(m), role))
	}
	var alter []string
	if st.CanLogin && !exp.login {
		d.Widening = append(d.Widening, "can log in although governance has it disabled")
		alter = append(alter, "NOLOGIN")
	}
	if st.ConnLimit < 0 || st.ConnLimit > exp.limit {
		d.Widening = append(d.Widening, fmt.Sprintf("connection limit %d is above the "+
			"recorded %d", st.ConnLimit, exp.limit))
		alter = append(alter, fmt.Sprintf("CONNECTION LIMIT %d", exp.limit))
	}
	if !st.CanLogin && exp.login {
		d.Narrowing = append(d.Narrowing, "cannot log in although governance has it active")
	}
	if st.ConnLimit >= 0 && st.ConnLimit < exp.limit {
		d.Narrowing = append(d.Narrowing, fmt.Sprintf("connection limit %d is below the "+
			"recorded %d", st.ConnLimit, exp.limit))
	}
	d.Fix = strings.Join(fix, "\n")
	var stmts []string
	if len(alter) > 0 {
		stmts = append(stmts, "ALTER ROLE "+role+" WITH "+strings.Join(alter, " "))
	}
	return d, stmts
}

// manualAttributeFixes are the statements a superuser or the owner runs
// for drift pg_sage may not correct.
func manualAttributeFixes(st agentguard.RoleState, role string) []string {
	var attrs []string
	for _, a := range []struct {
		on   bool
		attr string
	}{{st.Superuser, "NOSUPERUSER"}, {st.BypassRLS, "NOBYPASSRLS"},
		{st.Replication, "NOREPLICATION"}, {st.CreateRole, "NOCREATEROLE"},
		{st.CreateDB, "NOCREATEDB"}} {
		if a.on {
			attrs = append(attrs, a.attr)
		}
	}
	var out []string
	if len(attrs) > 0 {
		out = append(out, "ALTER ROLE "+role+" WITH "+strings.Join(attrs, " ")+";")
	}
	for _, m := range st.Dangerous {
		out = append(out, fmt.Sprintf("REVOKE %s FROM %s; -- or the role granting it",
			ident(m), role))
	}
	if st.Owned > 0 {
		out = append(out, "REASSIGN OWNED BY "+role+" TO <the owning role>; "+
			"-- in each database, then DROP OWNED BY "+role+";")
	}
	return out
}

// ident quotes an identifier when it needs it (agent role names never do).
func ident(name string) string {
	plain := name != ""
	for _, c := range name {
		lower, digit := c >= 'a' && c <= 'z', c >= '0' && c <= '9'
		if !lower && !digit && c != '_' {
			plain = false
			break
		}
	}
	if plain && (name[0] < '0' || name[0] > '9') {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
