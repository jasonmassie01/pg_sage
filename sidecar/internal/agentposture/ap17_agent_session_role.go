package agentposture

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

func init() { Register(ap17{}) }

// ap17 reports agent-like sessions (an application_name matching
// agents.client_patterns, as AP-13 classifies them) connected as a role
// that owns relations in a user schema, has BYPASSRLS or superuser, or is
// Supabase's service_role: the agent then acts as the owner or a service
// role, beyond any grant or row-level security. Registered agent roles are
// AP-01's and AP-02's. Critical, a warning on a database verified as not
// production. It names the role and the application, never the query.
type ap17 struct{}

func (ap17) Spec() Spec {
	return Spec{ID: "AP-17", Title: "Agent sessions as owner or service role",
		Severity: Critical, ReadsSessions: true}
}

// ap17Examples is how many owned relations a finding names.
const ap17Examples = 5

var ap17SQL = Statement("AP-17", `WITH s AS (
  SELECT DISTINCT a.usesysid, a.application_name::text AS app
  FROM pg_catalog.pg_stat_activity a
  WHERE `+clientSession+` AND a.usesysid <> $1 AND a.application_name <> ''
  ORDER BY 1, 2
  LIMIT $2)
SELECT r.oid, r.rolname::text, r.rolsuper, r.rolbypassrls, s.app,
  (SELECT count(*)::int FROM pg_catalog.pg_class c
   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
   WHERE c.relowner = r.oid AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
     AND `+apbUserSchemas+`),
  ARRAY(SELECT pg_catalog.quote_ident(n.nspname) || '.' || pg_catalog.quote_ident(c.relname)
   FROM pg_catalog.pg_class c
   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
   WHERE c.relowner = r.oid AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
     AND `+apbUserSchemas+`
   ORDER BY 1 LIMIT $3)
FROM s JOIN pg_catalog.pg_roles r ON r.oid = s.usesysid
ORDER BY r.rolname, s.app`)

// sessionRole is one role seen in agent-like sessions.
type sessionRole struct {
	oid           uint32
	name          string
	super, bypass bool
	owned         int
	examples      []string
	apps          []string
}

func (ap17) Detect(ctx context.Context, in Input) ([]Finding, error) {
	lim := in.capRows("sessions", maxHintSessions)
	rows, err := in.Q.Query(ctx, ap17SQL, in.Env.Self.OID, lim.fetch(), ap17Examples)
	if err != nil {
		return nil, fmt.Errorf("read agent sessions and their roles: %w", err)
	}
	defer rows.Close()
	var order []uint32
	byRole := map[uint32]*sessionRole{}
	for rows.Next() && !lim.past() {
		var s sessionRole
		var app string
		if err := rows.Scan(&s.oid, &s.name, &s.super, &s.bypass, &app, &s.owned,
			&s.examples); err != nil {
			return nil, fmt.Errorf("read agent sessions and their roles: %w", err)
		}
		if !ap17Candidate(in.Env, s.oid, app) {
			continue
		}
		if byRole[s.oid] == nil {
			byRole[s.oid] = &s
			order = append(order, s.oid)
		}
		byRole[s.oid].apps = append(byRole[s.oid].apps, app)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read agent sessions and their roles: %w", err)
	}
	var out []Finding
	for _, oid := range order {
		if s := byRole[oid]; len(s.reasons()) > 0 {
			out = append(out, ap17Finding(in.Env, *s))
		}
	}
	return out, nil
}

// ap17Candidate reports whether a session of role oid with app is an
// agent-like client AP-17 owns: not a registered agent role (AP-01 and
// AP-02 report those) and not pg_sage's action role.
func ap17Candidate(env Env, oid uint32, app string) bool {
	if !env.Config.MatchClient(app) {
		return false
	}
	if agent, ok := env.Agent(oid); ok && agent.Registered() {
		return false
	}
	return env.Action == nil || env.Action.OID != oid
}

// reasons lists why the role lets an agent act beyond its grants.
func (s sessionRole) reasons() []string {
	var out []string
	if s.super {
		out = append(out, "is a superuser")
	}
	if s.bypass {
		out = append(out, "has BYPASSRLS")
	}
	if s.name == "service_role" {
		out = append(out, "is Supabase's service_role")
	}
	if s.owned > 0 {
		out = append(out, fmt.Sprintf("owns %s in user schemas", plural(s.owned, "relation")))
	}
	return out
}

func ap17Finding(env Env, s sessionRole) Finding {
	sev := Critical
	if env.Config.NonProduction {
		sev = Warning
	}
	apps := slices.Clone(s.apps)
	slices.Sort(apps)
	why := joinAnd(s.reasons())
	ev := []Evidence{{Source: "pg_stat_activity", Ref: s.name,
		Detail: "application_name " + strings.Join(apps, ", ")}}
	for _, x := range s.examples {
		ev = append(ev, Evidence{Source: "pg_class", Ref: x, Detail: "owned by " + s.name})
	}
	role := sqlident.Ident(s.name)
	return Finding{Severity: sev, ObjectType: "role", Object: s.name,
		Title: fmt.Sprintf("Agent-like client connected as %s", s.name),
		Detail: fmt.Sprintf("Sessions with agent-like application_name %s connect as "+
			"role %s, which %s. The agent then acts as the owner or a service role: it "+
			"can bypass row-level security, change or drop the objects and re-grant them, "+
			"whatever its task needs.", strings.Join(apps, ", "), s.name, why),
		Recommendation: "Connect the agent with its own login (or a Guard principal) " +
			"that owns nothing, has NOBYPASSRLS and holds only the grants its task uses.",
		FixScript: "-- A dedicated login for the agent; grant it only what it needs:\n" +
			"CREATE ROLE " + sqlident.Ident(s.name+"_agent") + " LOGIN NOINHERIT " +
			"NOBYPASSRLS;\n-- then move the agent's connection string off " + role,
		Caveat: "Classified by application_name, which a client chooses; a registered " +
			"agent role is reported by AP-01 and AP-02 instead.",
		Evidence: ev}
}
