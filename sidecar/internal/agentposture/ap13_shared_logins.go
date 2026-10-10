package agentposture

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
)

func init() { Register(ap13{}) }

// ap13 reports one login role used at the same time by at least
// sharedLoginClients distinct clients (application_name and client
// address) when one of them looks like an agent: the agent then acts
// with whatever the shared login can do, and nothing tells its actions
// apart. Info, a warning once any Guard principal exists. Client
// addresses of other roles' sessions need pg_read_all_stats; without it
// the check counts application_name only and says so.
type ap13 struct{}

func (ap13) Spec() Spec {
	return Spec{ID: "AP-13", Title: "Login roles shared with agent-like clients",
		Severity: Warning}
}

// sharedLoginClients is how many distinct clients make a login shared.
const sharedLoginClients = 3

var ap13SQL = Statement("AP-13", `SELECT a.usesysid, a.usename::text,
  COALESCE(a.application_name, ''), COALESCE(pg_catalog.host(a.client_addr), ''),
  pg_catalog.pg_has_role(current_user, 'pg_read_all_stats', 'USAGE')
FROM pg_catalog.pg_stat_activity a
WHERE (a.backend_type = 'client backend' OR a.backend_type IS NULL)
  AND a.usesysid IS NOT NULL AND a.usesysid <> $1
ORDER BY a.usesysid, 3, 4
LIMIT $2`)

// loginClients is one role's distinct clients.
type loginClients struct {
	name    string
	clients map[string]bool // "app@addr", or "app" when degraded
	agently []string        // agent-like application names
}

func (ap13) Detect(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, ap13SQL, in.Env.Self.OID, maxHintSessions)
	if err != nil {
		return nil, fmt.Errorf("read sessions per login: %w", err)
	}
	defer rows.Close()
	byRole := map[uint32]*loginClients{}
	stats := true
	for rows.Next() {
		var oid uint32
		var name, app, addr string
		if err := rows.Scan(&oid, &name, &app, &addr, &stats); err != nil {
			return nil, fmt.Errorf("read sessions per login: %w", err)
		}
		lc := byRole[oid]
		if lc == nil {
			lc = &loginClients{name: name, clients: map[string]bool{}}
			byRole[oid] = lc
		}
		lc.add(in.Env, oid, app, addr, stats)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sessions per login: %w", err)
	}
	return sharedLoginFindings(in.Env, byRole, stats), nil
}

func (lc *loginClients) add(env Env, oid uint32, app, addr string, stats bool) {
	key := app
	if stats {
		key = app + "@" + addr
	}
	lc.clients[key] = true
	agent, _ := env.Agent(oid)
	if (env.Config.MatchClient(app) || agent.Registered()) && !slices.Contains(lc.agently, app) {
		lc.agently = append(lc.agently, app)
	}
}

func sharedLoginFindings(env Env, byRole map[uint32]*loginClients, stats bool) []Finding {
	sev := Info
	if env.PrincipalsExist {
		sev = Warning
	}
	oids := make([]uint32, 0, len(byRole))
	for oid := range byRole {
		oids = append(oids, oid)
	}
	sort.Slice(oids, func(i, j int) bool { return byRole[oids[i]].name < byRole[oids[j]].name })
	var out []Finding
	for _, oid := range oids {
		lc := byRole[oid]
		if len(lc.clients) < sharedLoginClients || len(lc.agently) == 0 {
			continue
		}
		out = append(out, sharedLoginFinding(lc, sev, stats))
	}
	return out
}

func sharedLoginFinding(lc *loginClients, sev Severity, stats bool) Finding {
	clients := make([]string, 0, len(lc.clients))
	for c := range lc.clients {
		clients = append(clients, c)
	}
	sort.Strings(clients)
	f := Finding{Severity: sev, ObjectType: "role", Object: lc.name,
		Title: "Login shared with an agent-like client",
		Detail: fmt.Sprintf("%d distinct clients use login %s at once (%s), including "+
			"agent-like %s: the agent acts with everything the login can do, and its "+
			"actions cannot be told apart.", len(clients), lc.name,
			listSome(clients, 10), strings.Join(lc.agently, ", ")),
		Recommendation: "Give the agent its own login (or a Guard principal) with only " +
			"the privileges it needs, and keep this login for the application.",
		FixScript: "-- A dedicated login for the agent; grant it only what it needs:\n" +
			"CREATE ROLE " + QuoteIdent(lc.name+"_agent") + " LOGIN NOINHERIT;\n" +
			"-- then move the agent's connection string to it",
		Evidence: []Evidence{{Source: "pg_stat_activity", Ref: lc.name,
			Detail: strings.Join(clients, ", ")}}}
	if !stats {
		f.Caveat = "pg_sage's role lacks pg_read_all_stats, so other roles' client " +
			"addresses are hidden: clients are counted by application_name only. " +
			"GRANT pg_read_all_stats TO <pg_sage role> to count addresses too."
	}
	return f
}
