package agentposture

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// identityRoles is a table owner role owning one fixture table, an
// observation role and an action role, all plain NOLOGIN roles.
type identityRoles struct{ owner, obs, act string }

func newIdentityRoles(f *fixture, obsAttrs, actAttrs string) identityRoles {
	r := identityRoles{owner: f.role("powner_", "NOLOGIN"),
		obs: f.role("pobs_", "NOLOGIN "+obsAttrs), act: f.role("pact_", "NOLOGIN "+actAttrs)}
	f.exec("CREATE TABLE "+f.q("owned")+" (id int)",
		"ALTER TABLE "+f.q("owned")+" OWNER TO "+r.owner,
		"GRANT "+r.owner+" TO "+r.act, "GRANT pg_signal_backend TO "+r.act)
	return r
}

func (r identityRoles) split(f *fixture) Env {
	return f.env(func(e *Env) {
		e.Self = selfRole(f, r.obs)
		a := selfRole(f, r.act)
		a.Source = SourceAction
		e.Action = &a
	})
}

func (r identityRoles) single(f *fixture) Env {
	return f.env(func(e *Env) { e.Self = selfRole(f, r.obs) })
}

// Happy path: a clean observation role and an action role that holds the
// owner membership and pg_signal_backend report nothing: acting rights
// are the action role's job.
func TestAP14_SplitIdentityCleanNoFinding(t *testing.T) {
	f := newFixture(t)
	r := newIdentityRoles(f, "IN ROLE pg_monitor", "")
	o := f.run("AP-14", r.split(f))
	requireNoFinding(t, o, r.obs)
	requireNoFinding(t, o, r.act)
}

// The observation role with owner membership is a warning that names the
// owner role and how to take it away.
func TestAP14_ObservationRoleOwnerMembershipWarns(t *testing.T) {
	f := newFixture(t)
	r := newIdentityRoles(f, "IN ROLE pg_monitor", "")
	f.exec("GRANT " + r.owner + " TO " + r.obs)
	got := requireFinding(t, f.run("AP-14", r.split(f)), r.obs, Warning)
	requireContains(t, "AP-14 detail", got.Detail, "observation", "owner", r.owner)
	requireContains(t, "AP-14 fix", got.FixScript, "REVOKE "+r.owner+" FROM "+r.obs+";")
}

// The observation role with pg_signal_backend is a warning.
func TestAP14_ObservationRoleSignalBackendWarns(t *testing.T) {
	f := newFixture(t)
	r := newIdentityRoles(f, "IN ROLE pg_monitor, pg_signal_backend", "")
	got := requireFinding(t, f.run("AP-14", r.split(f)), r.obs, Warning)
	requireContains(t, "AP-14 detail", got.Detail, "pg_signal_backend")
	requireContains(t, "AP-14 fix", got.FixScript,
		"REVOKE pg_signal_backend FROM "+r.obs+";")
	if strings.Contains(got.Detail, r.owner) {
		t.Fatalf("detail %q names an owner membership the role does not hold", got.Detail)
	}
}

// AP-14 checks both roles: an action role with BYPASSRLS is reported on
// the action role.
func TestAP14_ActionRoleBypassRLSWarns(t *testing.T) {
	f := newFixture(t)
	r := newIdentityRoles(f, "", "BYPASSRLS")
	o := f.run("AP-14", r.split(f))
	got := requireFinding(t, o, r.act, Warning)
	requireContains(t, "AP-14 action detail", got.Detail, "BYPASSRLS", "action")
	requireContains(t, "AP-14 action fix", got.FixScript,
		"ALTER ROLE "+r.act+" NOBYPASSRLS;")
	requireNoFinding(t, o, r.obs)
}

// Without action_dsn, one role that holds acting rights is reported as
// info ("one identity"), not as a warning: that is today's supported
// single-role deployment.
func TestAP14_OneIdentityIsInfo(t *testing.T) {
	f := newFixture(t)
	r := newIdentityRoles(f, "IN ROLE pg_signal_backend", "")
	f.exec("GRANT " + r.owner + " TO " + r.obs)
	got := requireFinding(t, f.run("AP-14", r.single(f)), r.obs, Info)
	requireContains(t, "AP-14 one identity title", strings.ToLower(got.Title),
		"one identity")
	requireContains(t, "AP-14 one identity recommendation", got.Recommendation,
		"action_dsn")
	requireContains(t, "AP-14 one identity detail", got.Detail, r.owner,
		"pg_signal_backend")
}

// State transitions: the same roles are a warning while action_dsn is set
// and turn into the one-identity note when it is removed.
func TestAP14_RemovingActionIdentityTurnsWarningIntoInfo(t *testing.T) {
	f := newFixture(t)
	r := newIdentityRoles(f, "", "")
	f.exec("GRANT " + r.owner + " TO " + r.obs)
	requireFinding(t, f.run("AP-14", r.split(f)), r.obs, Warning)
	requireFinding(t, f.run("AP-14", r.single(f)), r.obs, Info)
}

// Nil/empty: one identity without acting rights reports nothing (the
// observe-only deployment).
func TestAP14_OneIdentityWithoutActingRightsNoFinding(t *testing.T) {
	f := newFixture(t)
	r := newIdentityRoles(f, "IN ROLE pg_monitor", "")
	requireNoFinding(t, f.run("AP-14", r.single(f)), r.obs)
}

// ResolveEnv reads the configured action role; an unknown or same-as-self
// role resolves to no action identity.
func TestResolveEnv_ActionRole(t *testing.T) {
	f := newFixture(t)
	act := f.role("pact_", "NOLOGIN")
	resolve := func(role string) Env {
		cfg := DefaultConfig()
		cfg.ActionRole = role
		var env Env
		var err error
		readTx(t, f.ctx, f.pool, func(tx pgx.Tx) { env, err = ResolveEnv(f.ctx, tx, cfg) })
		if err != nil {
			t.Fatalf("ResolveEnv(%q): %v", role, err)
		}
		return env
	}
	env := resolve(act)
	if env.Action == nil || env.Action.Name != act || env.Action.OID == 0 ||
		env.Action.Source != SourceAction {
		t.Fatalf("Action = %+v, want %s", env.Action, act)
	}
	if env.Action.OID != selfRole(f, act).OID {
		t.Fatalf("Action OID = %d, want %d", env.Action.OID, selfRole(f, act).OID)
	}
	if env := resolve("pact_missing_" + suffix(t)); env.Action != nil {
		t.Fatalf("unknown action role resolved to %+v", env.Action)
	}
	if env := resolve(env.Self.Name); env.Action != nil {
		t.Fatalf("the observation role as action role resolved to %+v", env.Action)
	}
	if env := resolve(""); env.Action != nil {
		t.Fatalf("no action role resolved to %+v", env.Action)
	}
}
