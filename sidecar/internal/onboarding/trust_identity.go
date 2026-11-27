package onboarding

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// Identities a grant script is for (security-09).
const (
	IdentitySingle      = "single"      // one role observes and acts
	IdentityObservation = "observation" // the main role, with action_dsn set
	IdentityAction      = "action"      // the action_dsn role
)

// GrantReadAllStats names the action role's read of other sessions.
const GrantReadAllStats = "pg_read_all_stats"

// GrantScript is one role's "Grant more" script. Role is the role name as
// read ("" when unknown; the SQL then names the documented role).
type GrantScript struct {
	Identity string `json:"identity"`
	Role     string `json:"role"`
	SQL      string `json:"sql"`
}

// acting is the grant status and quoted role the acting levels use: the
// action identity's when there is one, else the one role's.
func (in GuideInput) acting() (GrantStatus, string) {
	if in.ActionGrants == nil {
		return in.Grants, quoteRole(in.Grants.Role)
	}
	g := *in.ActionGrants
	role := "sage_action"
	if g.Role != "" {
		role = sqlident.Role(g.Role)
	}
	return g, role
}

// GrantScripts is "Grant more": one script for one role, or two with an
// action identity, where the acting grants go only to the action role and
// the observation role keeps pg_monitor and the sage schema.
func GrantScripts(in GuideInput) []GrantScript {
	levels := TrustGuide(in)
	obs := levelNamed(levels, LevelObservation).Grants
	acts := levelNamed(levels, LevelAutonomous).Grants // a superset of advisory's
	if in.ActionGrants == nil {
		return []GrantScript{{Identity: IdentitySingle, Role: in.Grants.Role,
			SQL: scriptSQL(append(append([]Grant{}, obs...), acts...))}}
	}
	_, role := in.acting()
	read := Grant{Name: GrantReadAllStats, SQL: "GRANT pg_read_all_stats TO " + role +
		"; -- match a session's query and start before signalling it"}
	obsSQL := scriptSQL(obs) + "\n-- never grant table owner roles or the signal role " +
		"to this role: actions run as the action_dsn role"
	return []GrantScript{
		{Identity: IdentityObservation, Role: in.Grants.Role, SQL: obsSQL},
		{Identity: IdentityAction, Role: in.ActionGrants.Role,
			SQL: scriptSQL(append([]Grant{read}, acts...))},
	}
}

// scriptSQL joins the grants' SQL, each statement once.
func scriptSQL(grants []Grant) string {
	seen := map[string]bool{}
	var lines []string
	for _, g := range grants {
		if g.SQL == "" || seen[g.SQL] {
			continue
		}
		seen[g.SQL] = true
		lines = append(lines, g.SQL)
	}
	return strings.Join(lines, "\n")
}

func levelNamed(levels []Level, name string) Level {
	for _, l := range levels {
		if l.Level == name {
			return l
		}
	}
	return Level{}
}
