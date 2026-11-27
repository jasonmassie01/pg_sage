package onboarding

import (
	"strings"
	"testing"
)

// No concurrent access tests: GrantScripts and TrustGuide are pure
// functions of their input.

func splitInput() GuideInput {
	in := guideInput("advisory")
	in.Grants.Role = "sage_observer"
	in.Grants.Owners = []TableOwner{{Role: "app_owner", Tables: 4}}
	in.Grants.OwnerRoles = 1
	in.ActionGrants = &GrantStatus{Role: "sage_actor", SignalBackend: bp(true),
		Owners: []TableOwner{{Role: "app_owner", Tables: 4}}, OwnerRoles: 1,
		TablesOwned: 0, TablesTotal: 4, AlterSystem: bp(false)}
	return in
}

func script(scripts []GrantScript, identity string) GrantScript {
	for _, s := range scripts {
		if s.Identity == identity {
			return s
		}
	}
	return GrantScript{}
}

// Nil/empty: without an action identity "Grant more" is one script for the
// one role, with every grant in it (today's behaviour).
func TestGrantScripts_SingleIdentityIsOneScript(t *testing.T) {
	in := guideInput("advisory")
	in.Grants.Owners = []TableOwner{{Role: "app_owner", Tables: 4}}
	scripts := GrantScripts(in)
	if len(scripts) != 1 || scripts[0].Identity != IdentitySingle ||
		scripts[0].Role != "sage_agent" {
		t.Fatalf("scripts = %+v, want one single-identity script for sage_agent", scripts)
	}
	sql := scripts[0].SQL
	for _, want := range []string{"GRANT pg_monitor TO sage_agent;",
		"GRANT app_owner TO sage_agent;", "pg_signal_backend"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("single script lacks %q:\n%s", want, sql)
		}
	}
}

// Happy path: with an action identity "Grant more" is two scripts, one
// per role, and the acting grants go only to the action role.
func TestGrantScripts_SplitIdentityIsTwoScripts(t *testing.T) {
	scripts := GrantScripts(splitInput())
	if len(scripts) != 2 {
		t.Fatalf("scripts = %+v, want two", scripts)
	}
	obs, act := script(scripts, IdentityObservation), script(scripts, IdentityAction)
	if obs.Role != "sage_observer" || act.Role != "sage_actor" {
		t.Fatalf("roles = %q, %q; want sage_observer, sage_actor", obs.Role, act.Role)
	}
	for _, want := range []string{"GRANT pg_monitor TO sage_observer;",
		"CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION sage_observer;"} {
		if !strings.Contains(obs.SQL, want) {
			t.Fatalf("observation script lacks %q:\n%s", want, obs.SQL)
		}
	}
	for _, never := range []string{"GRANT app_owner", "GRANT pg_signal_backend",
		"pg_maintain", "ALTER SYSTEM"} {
		if strings.Contains(obs.SQL, never) {
			t.Fatalf("observation script grants %q:\n%s", never, obs.SQL)
		}
	}
	if !strings.Contains(act.SQL, "GRANT app_owner TO sage_actor;") {
		t.Fatalf("action script lacks the owner membership:\n%s", act.SQL)
	}
	if strings.Contains(act.SQL, "sage_observer") || strings.Contains(act.SQL, "pg_monitor") {
		t.Fatalf("action script addresses the observation role or pg_monitor:\n%s", act.SQL)
	}
}

// The trust guide's acting levels address and check the action role.
func TestTrustGuide_SplitIdentityActingLevelsUseTheActionRole(t *testing.T) {
	levels := TrustGuide(splitInput())
	obs := level(levels, LevelObservation)
	if g := grant(obs, GrantMonitor); !strings.Contains(g.SQL, "sage_observer") {
		t.Fatalf("observation pg_monitor grant = %q", g.SQL)
	}
	adv := level(levels, LevelAdvisory)
	sig := grant(adv, GrantSignalBackend)
	if !strings.Contains(sig.SQL, "sage_actor") || sig.Present == nil || !*sig.Present {
		t.Fatalf("advisory pg_signal_backend = %+v, want the action role's, present", sig)
	}
	owner := grant(adv, GrantTableOwnership+":app_owner")
	if owner.SQL != "GRANT app_owner TO sage_actor;" {
		t.Fatalf("advisory owner grant = %q", owner.SQL)
	}
	auto := level(levels, LevelAutonomous)
	if g := grant(auto, GrantAlterSystem); !strings.Contains(g.SQL, "TO sage_actor") {
		t.Fatalf("autonomous ALTER SYSTEM grant = %q", g.SQL)
	}
}

// Boundary: an action identity whose role name is unknown reads as the
// documented sage_action role, never as the observation role.
func TestGrantScripts_UnknownActionRoleName(t *testing.T) {
	in := splitInput()
	in.ActionGrants.Role = ""
	act := script(GrantScripts(in), IdentityAction)
	if act.Role != "" || !strings.Contains(act.SQL, "TO sage_action;") ||
		strings.Contains(act.SQL, "sage_observer") {
		t.Fatalf("action script for an unknown role = %+v", act)
	}
}
