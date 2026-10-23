package pgaudit

import (
	"regexp"
	"strconv"

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
)

var (
	// agentApp is 'pg_sage agent:<principal>[:<action>]' (spec §6.6, §6.17);
	// the principal is its id (agp_...) or its name slug.
	agentApp = regexp.MustCompile(
		`^pg_sage agent:(agp_[a-z2-7]{20}|[a-z][a-z0-9-]{1,62})(?::([0-9]{1,18}))?$`)
)

// Attribution is whom a log entry belongs to, as far as pg_sage can tell.
type Attribution struct {
	By        string
	Principal string // id or name from application_name
	Role      string // the agent role, when attributed by role
	ActionID  int64
}

// Attribute decides whether e belongs to an agent principal or to pg_sage.
// Other sessions are not pg_sage's to keep.
func Attribute(e logwatch.LogEntry) (Attribution, bool) {
	if m := agentApp.FindStringSubmatch(e.Application); m != nil {
		a := Attribution{By: ByApplication, Principal: m[1]}
		if m[2] != "" {
			a.ActionID, _ = strconv.ParseInt(m[2], 10, 64)
		}
		return a, true
	}
	if e.Application == selfmonitor.ApplicationName {
		return Attribution{By: ByPGSage}, true
	}
	// An agent's broker or direct-lane role: the same names agentguard
	// creates (sage_agent_/sage_agentb_ and ten lower base32 characters).
	if agentguard.IsAgentRoleName(e.User) {
		return Attribution{By: ByRole, Role: e.User}, true
	}
	return Attribution{}, false
}
