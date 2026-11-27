package pgaudit

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// How a record was correlated (stored as correlated_by).
const (
	ByApplication = "application_name" // a brokered agent connection
	ByPGSage      = "pg_sage"          // pg_sage's own connection
	ByStatement   = "statement"        // pg_sage's own, linked to an action by its SQL
	ByRole        = "role"             // an agent role connected directly
	// ByActionTag is pg_sage's own session running one decided action,
	// named 'pg_sage action:<decision id>' by the executor (audit-06).
	ByActionTag = "action_tag"
	// ByApplicationUnverified is an agent's application_name set by a role
	// that is not that principal's own: kept for forensics, attributed to
	// no one.
	ByApplicationUnverified = "application_name_unverified"
)

var (
	// agentApp is 'pg_sage agent:<principal>[:<action>]' (spec §6.6, §6.17);
	// the principal is its id (agp_...) or its name slug.
	agentApp = regexp.MustCompile(
		`^pg_sage agent:(agp_[a-z2-7]{20}|[a-z][a-z0-9-]{1,62})(?::([0-9]{1,18}))?$`)
)

// Attribution is whom a log entry belongs to, as far as pg_sage can tell.
type Attribution struct {
	By string
	// Principal is the principal's id (agp_...), never its name; empty when
	// the session's role is not known to be that principal's.
	Principal string
	Role      string // the agent role, when attributed by role
	ActionID  int64
	// DecisionID is the decision an action tag names (ByActionTag only).
	DecisionID int64
}

// Attribute decides whether e belongs to an agent principal or to pg_sage.
// Any session can set application_name, so the role it logged in as
// decides: an agent's claim counts only from that principal's own roles,
// and pg_sage's only from ownRole (pg_sage's own login role; empty matches
// nothing). roles maps agent roles to their principals (nil: unknown).
// Other sessions are not pg_sage's to keep.
func Attribute(e logwatch.LogEntry, ownRole string, roles RoleResolver) (Attribution, bool) {
	if m := agentApp.FindStringSubmatch(e.Application); m != nil {
		return attributeClaim(e.User, m[1], m[2], roles), true
	}
	if id, ok := selfmonitor.ParseActionApplicationName(e.Application); ok &&
		ownRole != "" && e.User == ownRole {
		return Attribution{By: ByActionTag, DecisionID: id}, true
	}
	if e.Application == selfmonitor.ApplicationName && ownRole != "" && e.User == ownRole {
		return Attribution{By: ByPGSage}, true
	}
	// An agent's broker or direct-lane role: the same names agentguard
	// creates (sage_agent_/sage_agentb_ and ten lower base32 characters).
	if agentguard.IsAgentRoleName(e.User) {
		a := Attribution{By: ByRole, Role: e.User}
		if o, ok := principalOfRole(roles, e.User); ok {
			a.Principal = o.ID
		}
		return a, true
	}
	return Attribution{}, false
}

// attributeClaim checks an agent application name's claim against the
// session's role: an id claim must be one of that id's two roles (a pure
// hash, no lookup); a name claim must be the name of the role's principal,
// whose id is stored. Anything else is kept unverified.
func attributeClaim(user, claim, action string, roles RoleResolver) Attribution {
	id, ok := claimedPrincipal(user, claim, roles)
	if !ok {
		return Attribution{By: ByApplicationUnverified}
	}
	a := Attribution{By: ByApplication, Principal: id}
	if action != "" {
		a.ActionID, _ = strconv.ParseInt(action, 10, 64)
	}
	return a
}

func claimedPrincipal(user, claim string, roles RoleResolver) (string, bool) {
	if !agentguard.IsAgentRoleName(user) {
		return "", false
	}
	if strings.HasPrefix(claim, "agp_") {
		own := user == agentguard.LoginRoleName(claim) ||
			user == agentguard.BrokerRoleName(claim)
		return claim, own
	}
	o, ok := principalOfRole(roles, user)
	if !ok || o.Name != claim {
		return "", false
	}
	return o.ID, true
}

// principalOfRole asks roles, when there are any, for role's principal.
func principalOfRole(roles RoleResolver, role string) (Owner, bool) {
	if roles == nil {
		return Owner{}, false
	}
	return roles.PrincipalOfRole(role)
}
