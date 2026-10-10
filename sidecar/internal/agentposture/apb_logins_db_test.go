package agentposture

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

const loginSecret = "posture-fixture-secret"

// sharedLogin creates a login role and opens one session per app.
func sharedLogin(f *fixture, apps ...string) string {
	f.t.Helper()
	role := f.role("pshare_", "LOGIN PASSWORD '"+loginSecret+"'")
	for _, app := range apps {
		loginAs(f.t, f.ctx, role, loginSecret, app)
	}
	return role
}

// withClients sets the client patterns AP-13 and AP-16 match.
func withClients(e *Env) { e.Config.ClientPatterns = []string{"^claude", "^cursor"} }

// runAs runs detector id inside a read-only transaction under SET ROLE
// role, as a pg_sage role without pg_read_all_stats would.
func runAs(f *fixture, id, role string, env Env) Outcome {
	f.t.Helper()
	d, _ := Default().Get(id)
	var out Outcome
	var err error
	readTx(f.t, f.ctx, f.pool, func(tx pgx.Tx) {
		if _, err = tx.Exec(f.ctx, "SET LOCAL ROLE "+role); err != nil {
			return
		}
		out, err = RunDetector(f.ctx, d, tx, env)
	})
	if err != nil {
		f.t.Fatalf("%s as %s: %v", id, role, err)
	}
	return out
}

// AP-13: one login used by three clients, one agent-like: info before any
// principal exists, warning after.
func TestAP13_SharedLoginWithAgentClient(t *testing.T) {
	f := newFixture(t)
	role := sharedLogin(f, "claude-code", "billing", "reports")
	env := f.env(withClients)
	got := requireFinding(t, f.run("AP-13", env), role, Info)
	requireContains(t, "AP-13 detail", got.Detail, "claude-code", "billing", "reports", "3")
	if got.Caveat != "" {
		t.Fatalf("AP-13 with pg_read_all_stats must not degrade: caveat %q", got.Caveat)
	}

	agent := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, agent, "NOLOGIN")
	env = f.env(func(e *Env) { withClients(e); f.agent(e, agent, SourceRegistered) })
	requireFinding(t, f.run("AP-13", env), role, Warning)
}

func TestAP13_BelowThreeClientsOrNoAgentClient(t *testing.T) {
	f := newFixture(t)
	two := sharedLogin(f, "claude-code", "billing")
	plain := sharedLogin(f, "billing", "reports", "etl")
	o := f.run("AP-13", f.env(withClients))
	requireNoFinding(t, o, two)
	requireNoFinding(t, o, plain)
}

// Without pg_read_all_stats AP-13 counts application_name only and says why.
func TestAP13_DegradesWithoutReadAllStats(t *testing.T) {
	f := newFixture(t)
	role := sharedLogin(f, "cursor-agent", "billing", "reports")
	low := f.role("plow_", "NOLOGIN")
	got := requireFinding(t, runAs(f, "AP-13", low, f.env(withClients)), role, Info)
	requireContains(t, "AP-13 degraded caveat", got.Caveat, "pg_read_all_stats",
		"application_name")
}

// AP-14: pg_sage's own role. The test servers run as a superuser, which
// bypasses row-level security and holds every role: a warning.
func TestAP14_SuperuserSelf(t *testing.T) {
	f := newFixture(t)
	env := f.env(nil)
	got := requireFinding(t, f.run("AP-14", env), env.Self.Name, Warning)
	requireContains(t, "AP-14 detail", got.Detail, "superuser")
	if got.ObjectType != "role" {
		t.Fatalf("AP-14 object type = %q", got.ObjectType)
	}
}

func TestAP14_BypassRLSAndInheritedAgent(t *testing.T) {
	f := newFixture(t)
	self := f.role("pself_", "NOLOGIN BYPASSRLS")
	agent := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, agent, "NOLOGIN")
	f.exec("GRANT " + agent + " TO " + self)
	env := f.env(func(e *Env) {
		e.Self = selfRole(f, self)
		f.agent(e, agent, SourceRegistered)
	})
	o := f.run("AP-14", env)
	got := requireFinding(t, o, self, Warning)
	requireContains(t, "AP-14 detail", got.Detail, "BYPASSRLS", agent)
	requireContains(t, "AP-14 fix", got.FixScript, "ALTER ROLE "+self+" NOBYPASSRLS")
	if env.VersionNum >= 160000 {
		requireContains(t, "AP-14 fix", got.FixScript,
			"REVOKE INHERIT OPTION FOR "+agent+" FROM "+self)
		requireSkipped(t, o, "inherit_role_level")
	} else {
		requireContains(t, "AP-14 fix", got.FixScript, "REVOKE "+agent+" FROM "+self)
		requireSkipped(t, o, "inherit_per_grant")
	}
}

func TestAP14_CleanSelfNoFinding(t *testing.T) {
	f := newFixture(t)
	self := f.role("pself_", "NOLOGIN")
	agent := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, agent, "NOLOGIN")
	env := f.env(func(e *Env) {
		e.Self = selfRole(f, self)
		f.agent(e, agent, SourceRegistered)
	})
	requireNoFinding(t, f.run("AP-14", env), self)
}

// PG16+: a membership granted WITH INHERIT FALSE (how Guard grants itself
// agent roles, createrole_self_grant = 'set') is not inheritance.
func TestAP14_NoInheritGrantIsFine(t *testing.T) {
	f := newFixture(t)
	if f.env(nil).VersionNum < 160000 {
		t.Skip("per-grant INHERIT exists from PostgreSQL 16; PG14/15 use the role-level arm")
	}
	self := f.role("pself_", "NOLOGIN")
	agent := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, agent, "NOLOGIN")
	f.exec("GRANT " + agent + " TO " + self + " WITH INHERIT FALSE, SET TRUE")
	env := f.env(func(e *Env) {
		e.Self = selfRole(f, self)
		f.agent(e, agent, SourceRegistered)
	})
	requireNoFinding(t, f.run("AP-14", env), self)
}

func selfRole(f *fixture, name string) Role {
	f.t.Helper()
	var oid uint32
	if err := f.pool.QueryRow(f.ctx, "SELECT oid FROM pg_roles WHERE rolname = $1",
		name).Scan(&oid); err != nil {
		f.t.Fatalf("role %s: %v", name, err)
	}
	return Role{OID: oid, Name: name, Source: SourceSelf}
}

// AP-16: agent-like logins Guard does not manage, once a principal exists.
func TestAP16_UnmanagedAgentLogins(t *testing.T) {
	f := newFixture(t)
	hint := sharedLogin(f, "claude-desktop")
	before := f.env(func(e *Env) { f.agent(e, hint, SourceClientHint) })
	if before.PrincipalsExist {
		t.Fatalf("fixture: a registered agent role already exists: %+v", before.Agents)
	}
	requireNoFinding(t, f.run("AP-16", before), hint)

	agent := registeredRoleName(t) // the first principal
	createRole(t, f.ctx, f.pool, agent, "NOLOGIN")

	after := f.env(func(e *Env) {
		f.agent(e, hint, SourceClientHint)
		f.agent(e, agent, SourceRegistered)
	})
	o := f.run("AP-16", after)
	got := requireFinding(t, o, hint, Warning)
	requireContains(t, "AP-16 fix", got.FixScript, "ALTER ROLE "+hint+" NOLOGIN")
	requireNoFinding(t, o, agent)
}

// AP-16 skips a hinted role that cannot log in (seen through SET ROLE).
func TestAP16_NonLoginHintIsNotALogin(t *testing.T) {
	f := newFixture(t)
	hint := f.role("pnolog_", "NOLOGIN")
	agent := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, agent, "NOLOGIN")
	env := f.env(func(e *Env) {
		f.agent(e, hint, SourceClientHint)
		f.agent(e, agent, SourceRegistered)
	})
	requireNoFinding(t, f.run("AP-16", env), hint)
}
