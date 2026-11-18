package agentposture

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// AP-17 (fleetpol-02a): agent-like sessions connected as a table owner,
// a BYPASSRLS role or Supabase's service_role.

// agentSession creates a login role with attrs, opens a session as it
// with application_name app and runs a query carrying marker.
func agentSession(f *fixture, prefix, attrs, app, marker string) string {
	f.t.Helper()
	role := f.role(prefix, "LOGIN PASSWORD '"+loginSecret+"' "+attrs)
	conn := loginAs(f.t, f.ctx, role, loginSecret, app)
	if _, err := conn.Exec(f.ctx, "SELECT '"+marker+"'::text"); err != nil {
		f.t.Fatalf("query as %s: %v", role, err)
	}
	return role
}

// hintEnv is the fixture environment with the shipped client patterns
// (AP-17 classifies sessions like AP-13 does).
func (f *fixture) hintEnv(mut func(*Env)) Env {
	return f.env(func(e *Env) {
		e.Config.ClientPatterns = config.DefaultClientPatterns()
		if mut != nil {
			mut(e)
		}
	})
}

func TestAP17_AgentSessionAsTableOwnerIsCritical(t *testing.T) {
	f := newFixture(t)
	marker := "ap17-query-text-" + suffix(t)
	owner := agentSession(f, "pown_", "", "mcp-server-postgres", marker)
	f.exec("CREATE TABLE "+f.q("leads")+" (id int)",
		"ALTER TABLE "+f.q("leads")+" OWNER TO "+owner)
	o := f.run("AP-17", f.hintEnv(nil))
	got := requireFinding(t, o, owner, Critical)
	requireContains(t, "AP-17 detail", got.Detail, owner, "mcp-server-postgres",
		"owns 1 relation")
	for _, text := range []string{got.Detail, got.Title, got.FixScript, got.Caveat} {
		if strings.Contains(text, marker) {
			t.Fatalf("AP-17 reported query text: %q", text)
		}
	}
	for _, ev := range got.Evidence {
		if strings.Contains(ev.Detail+ev.Ref, marker) {
			t.Fatalf("AP-17 evidence carries query text: %+v", ev)
		}
	}
	// Verified non-production: the same session is a warning.
	o = f.run("AP-17", f.hintEnv(func(e *Env) { e.Config.NonProduction = true }))
	requireFinding(t, o, owner, Warning)
}

func TestAP17_PlainRoleAndNonAgentClientsAreClean(t *testing.T) {
	f := newFixture(t)
	plain := agentSession(f, "pplain_", "", "claude-code", "m")
	human := agentSession(f, "phuman_", "BYPASSRLS", "psql", "m")
	f.exec("CREATE TABLE "+f.q("owned")+" (id int)",
		"ALTER TABLE "+f.q("owned")+" OWNER TO "+human)
	o := f.run("AP-17", f.hintEnv(nil))
	requireNoFinding(t, o, plain)
	requireNoFinding(t, o, human)
}

func TestAP17_BypassRLSAgentSessionIsCritical(t *testing.T) {
	f := newFixture(t)
	svc := agentSession(f, "pbyp_", "BYPASSRLS", "cursor-agent", "m")
	got := requireFinding(t, f.run("AP-17", f.hintEnv(nil)), svc, Critical)
	requireContains(t, "AP-17 detail", got.Detail, "BYPASSRLS", "cursor-agent")
}

func TestAP17_ServiceRoleAgentSessionIsCritical(t *testing.T) {
	f := newFixture(t)
	testdb.HoldSupabaseRoles(t, testdb.SkipUnlessLive(t))
	f.exec("CREATE ROLE service_role LOGIN PASSWORD '" + loginSecret + "'")
	conn := loginAs(t, f.ctx, "service_role", loginSecret, "mcp-supabase")
	if _, err := conn.Exec(f.ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	got := requireFinding(t, f.run("AP-17", f.hintEnv(nil)), "service_role", Critical)
	requireContains(t, "AP-17 detail", got.Detail, "service_role", "mcp-supabase")
}

// A registered agent role is AP-01's and AP-02's: AP-17 does not report it
// again, while AP-02 does.
func TestAP17_RegisteredAgentRoleIsLeftToAP01AndAP02(t *testing.T) {
	f := newFixture(t)
	name := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, name, "LOGIN PASSWORD '"+loginSecret+"'")
	conn := loginAs(t, f.ctx, name, loginSecret, "mcp-guard")
	if _, err := conn.Exec(f.ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	f.exec("CREATE TABLE "+f.q("agent_owned")+" (id int)",
		"ALTER TABLE "+f.q("agent_owned")+" OWNER TO "+pgx.Identifier{name}.Sanitize())
	env := f.hintEnv(func(e *Env) { f.agent(e, name, SourceRegistered) })
	requireNoFinding(t, f.run("AP-17", env), name)
	requireFinding(t, f.run("AP-02", env), name, Critical)
}

// The shipped detector set: AP-17 is registered, declares critical and
// never runs outside the session arm it needs.
func TestAP17_Spec(t *testing.T) {
	d, ok := Default().Get("AP-17")
	if !ok {
		t.Fatal("AP-17 is not registered")
	}
	s := d.Spec()
	if s.Severity != Critical || !s.ReadsSessions || s.Title == "" {
		t.Fatalf("AP-17 spec = %+v", s)
	}
}
