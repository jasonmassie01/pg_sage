package agentposture

import (
	"context"
	"fmt"
	"slices"
)

// PublicOID stands for PUBLIC in role lists, as in aclexplode's grantee.
const PublicOID uint32 = 0

// RoleSource is why a role is exposed or counted as an agent.
type RoleSource string

// Role sources.
const (
	SourcePublic     RoleSource = "public"      // PUBLIC: every role
	SourceConfigured RoleSource = "configured"  // agents.exposed_roles
	SourceSupabase   RoleSource = "supabase"    // anon and authenticated, both present
	SourceRegistered RoleSource = "registered"  // a Guard agent role
	SourceClientHint RoleSource = "client_hint" // seen with an agent-like application_name
	SourceSelf       RoleSource = "self"        // pg_sage's own role
)

// Role is one database role in the posture environment.
type Role struct {
	OID    uint32
	Name   string
	Source RoleSource
	// Hint is the application_name a client-hint role was seen with.
	Hint string
}

// Registered reports whether r is a registered agent role (not a hint).
func (r Role) Registered() bool { return r.Source == SourceRegistered }

// Env is what every detector of one run shares: the server version,
// pg_sage's own role, the exposed roles and the agent roles.
type Env struct {
	VersionNum int
	Self       Role
	// Exposed is PUBLIC first, then agents.exposed_roles that exist, then
	// anon and authenticated when both exist.
	Exposed []Role
	// Missing lists agents.exposed_roles entries that do not exist.
	Missing []string
	// Agents is the registered agent roles, then the client-hint roles.
	Agents []Role
	// PrincipalsExist is true once any registered agent role exists; some
	// detectors raise their severity then (AP-13, AP-16).
	PrincipalsExist bool
	Config          Config
	// Observations keeps observations between runs (AP-12); nil in the
	// first look.
	Observations *ObservationStore
}

// ExposedOIDs lists the exposed role OIDs, PUBLIC (0) included.
func (e Env) ExposedOIDs() []uint32 { return oids(e.Exposed) }

// AgentOIDs lists every agent role OID, registered and hinted.
func (e Env) AgentOIDs() []uint32 { return oids(e.Agents) }

// RegisteredAgents lists the registered agent roles.
func (e Env) RegisteredAgents() []Role {
	return slices.DeleteFunc(slices.Clone(e.Agents), func(r Role) bool { return !r.Registered() })
}

// HintAgents lists the roles only hinted at by their client names.
func (e Env) HintAgents() []Role {
	return slices.DeleteFunc(slices.Clone(e.Agents), func(r Role) bool { return r.Registered() })
}

// Agent returns the agent role with oid.
func (e Env) Agent(oid uint32) (Role, bool) {
	for _, r := range e.Agents {
		if r.OID == oid {
			return r, true
		}
	}
	return Role{}, false
}

// Exposure returns the exposed role with oid (0 is PUBLIC).
func (e Env) Exposure(oid uint32) (Role, bool) {
	for _, r := range e.Exposed {
		if r.OID == oid {
			return r, true
		}
	}
	return Role{}, false
}

func oids(rs []Role) []uint32 {
	out := make([]uint32, len(rs))
	for i, r := range rs {
		out[i] = r.OID
	}
	return out
}

// registeredRolePattern is the Guard agent role naming of spec §6.6:
// sage_agent_ or sage_agentb_ and 10 lower base32 characters. Until
// principals exist (G1), a registered agent role is one with this name.
const registeredRolePattern = `^sage_agentb?_[a-z2-7]{10}$`

// maxHintSessions bounds the pg_stat_activity read.
const maxHintSessions = 5000

var selfSQL = Statement("env", `SELECT current_setting('server_version_num')::int,
  r.oid, r.rolname::text FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`)

var rolesSQL = Statement("env", `SELECT r.oid, r.rolname::text,
  r.rolname ~ '`+registeredRolePattern+`'
FROM pg_catalog.pg_roles r
WHERE r.rolname = ANY($1::text[]) OR r.rolname ~ '`+registeredRolePattern+`'
ORDER BY r.rolname`)

// clientSession keeps the client sessions of pg_stat_activity a. Without
// pg_read_all_stats, other roles' sessions show a NULL backend_type, but
// their usesysid, usename and application_name stay visible, so a NULL
// backend_type counts as a client (background workers have no usesysid).
// Every session read shares it: the client hints here and AP-13.
const clientSession = `(a.backend_type = 'client backend' OR a.backend_type IS NULL)
  AND a.usesysid IS NOT NULL`

// sessionsSQL reads who connects with which application_name.
var sessionsSQL = Statement("env", `SELECT DISTINCT a.usesysid, a.usename::text,
  a.application_name
FROM pg_catalog.pg_stat_activity a
WHERE `+clientSession+`
  AND a.application_name <> ''
ORDER BY a.usesysid, a.application_name
LIMIT $1`)

// ResolveEnv reads the server version, pg_sage's role and the exposed and
// agent roles for one run.
func ResolveEnv(ctx context.Context, q Querier, cfg Config) (Env, error) {
	if err := cfg.Validate(); err != nil {
		return Env{}, err
	}
	env := Env{Config: cfg, Self: Role{Source: SourceSelf}}
	if err := q.QueryRow(ctx, selfSQL).Scan(&env.VersionNum, &env.Self.OID,
		&env.Self.Name); err != nil {
		return Env{}, fmt.Errorf("agent posture: read server version and own role: %w", err)
	}
	if err := resolveRoles(ctx, q, cfg, &env); err != nil {
		return Env{}, err
	}
	if err := resolveHints(ctx, q, cfg, &env); err != nil {
		return Env{}, err
	}
	env.PrincipalsExist = len(env.RegisteredAgents()) > 0
	return env, nil
}

type namedRole struct {
	oid        uint32
	registered bool
}

// resolveRoles adds the exposed roles and the registered agent roles.
func resolveRoles(ctx context.Context, q Querier, cfg Config, env *Env) error {
	names := append(slices.Clone(cfg.ExposedRoles), "anon", "authenticated")
	rows, err := q.Query(ctx, rolesSQL, names)
	if err != nil {
		return fmt.Errorf("agent posture: read roles: %w", err)
	}
	defer rows.Close()
	found := map[string]namedRole{}
	for rows.Next() {
		var name string
		var r namedRole
		if err := rows.Scan(&r.oid, &name, &r.registered); err != nil {
			return fmt.Errorf("agent posture: read roles: %w", err)
		}
		found[name] = r
		if r.registered && name != env.Self.Name {
			env.Agents = append(env.Agents, Role{OID: r.oid, Name: name,
				Source: SourceRegistered})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("agent posture: read roles: %w", err)
	}
	env.Exposed = exposedRoles(cfg.ExposedRoles, found, &env.Missing)
	return nil
}

func exposedRoles(configured []string, found map[string]namedRole,
	missing *[]string) []Role {
	out := []Role{{OID: PublicOID, Name: "PUBLIC", Source: SourcePublic}}
	seen := map[string]bool{}
	add := func(name string, src RoleSource) {
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, Role{OID: found[name].oid, Name: name, Source: src})
	}
	for _, name := range configured {
		if _, ok := found[name]; !ok {
			if !slices.Contains(*missing, name) {
				*missing = append(*missing, name)
			}
			continue
		}
		add(name, SourceConfigured)
	}
	_, anon := found["anon"]
	_, auth := found["authenticated"]
	if anon && auth {
		add("anon", SourceSupabase)
		add("authenticated", SourceSupabase)
	}
	return out
}

// resolveHints adds the roles seen with an agent-like application_name
// that are not registered agent roles or pg_sage's own role.
func resolveHints(ctx context.Context, q Querier, cfg Config, env *Env) error {
	matchers := cfg.clientMatcher()
	if len(matchers) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, sessionsSQL, maxHintSessions)
	if err != nil {
		return fmt.Errorf("agent posture: read sessions for client hints: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.OID, &r.Name, &r.Hint); err != nil {
			return fmt.Errorf("agent posture: read sessions for client hints: %w", err)
		}
		if r.OID == env.Self.OID || !matchAny(matchers, r.Hint) {
			continue
		}
		if _, known := env.Agent(r.OID); known {
			continue
		}
		r.Source = SourceClientHint
		env.Agents = append(env.Agents, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("agent posture: read sessions for client hints: %w", err)
	}
	return nil
}
