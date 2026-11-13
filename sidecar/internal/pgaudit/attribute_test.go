package pgaudit

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// No concurrent access tests here: Attribute is a pure function of its
// arguments; the correlator's shared state is covered in correlate_db_test.go.

const ownRole = "sage"

func attributeCase(t *testing.T, app, user, role string, want Attribution, wantOK bool) {
	t.Helper()
	got, ok := Attribute(entry(app, user, ddl), role, nil)
	if ok != wantOK || got != want {
		t.Fatalf("app %q user %q own %q: got %+v ok=%v, want %+v ok=%v",
			app, user, role, got, ok, want, wantOK)
	}
}

// Invalid input: an agent application name from a role that is not an agent
// role is kept for forensics, but it names no principal and no action.
func TestAttributeSpoofedAgentAppIsUnverified(t *testing.T) {
	unverified := Attribution{By: ByApplicationUnverified}
	for _, user := range []string{"app_user", "mallory", ownRole, "postgres", "",
		"sage_agent_K2M4Q7X7AB", "sage_agentb_k2m4q7x9ab", "sage_agent_k2m4q7x7ab2"} {
		attributeCase(t, "pg_sage agent:ci-bot:77", user, ownRole, unverified, true)
		attributeCase(t, "pg_sage agent:agp_aaaaaaaaaaaaaaaaaaaa", user, ownRole,
			unverified, true)
	}
	if ByApplicationUnverified != "application_name_unverified" {
		t.Fatalf("correlated_by value = %q", ByApplicationUnverified)
	}
}

// Happy path: a principal's own role (either lane) with its id in the
// agent application name is attributed to that principal and action; any
// other agent role's claim is not (audit-04b).
func TestAttributeAgentRoleWithAgentApp(t *testing.T) {
	for _, user := range []string{agentguard.LoginRoleName(idA),
		agentguard.BrokerRoleName(idA)} {
		attributeCase(t, "pg_sage agent:"+idA+":77", user, ownRole,
			Attribution{By: ByApplication, Principal: idA, ActionID: 77}, true)
		attributeCase(t, "pg_sage agent:"+idA, user, ownRole,
			Attribution{By: ByApplication, Principal: idA}, true)
	}
	for _, user := range []string{"sage_agent_k2m4q7x7ab", "sage_agentb_abcdefgh23"} {
		attributeCase(t, "pg_sage agent:"+idA+":77", user, ownRole,
			Attribution{By: ByApplicationUnverified}, true)
	}
}

// Boundary: application_name pg_sage is pg_sage's own session only on
// pg_sage's own role; the match is exact, case and all.
func TestAttributePGSageAppNeedsOwnRole(t *testing.T) {
	attributeCase(t, "pg_sage", ownRole, ownRole, Attribution{By: ByPGSage}, true)
	for _, user := range []string{"mallory", "Sage", "sage ", "postgres", ""} {
		attributeCase(t, "pg_sage", user, ownRole, Attribution{}, false)
	}
	// Another role's session named pg_sage is still an agent role's.
	attributeCase(t, "pg_sage", "sage_agent_k2m4q7x7ab", ownRole,
		Attribution{By: ByRole, Role: "sage_agent_k2m4q7x7ab"}, true)
	// pg_sage's role under another application name is not pg_sage's work.
	attributeCase(t, "psql", ownRole, ownRole, Attribution{}, false)
}

// Nil/empty: an unknown own role never makes anything pg_sage's.
func TestAttributeEmptyOwnRoleNeverMatchesPGSage(t *testing.T) {
	for _, user := range []string{"", "sage", "postgres"} {
		attributeCase(t, "pg_sage", user, "", Attribution{}, false)
	}
	attributeCase(t, "", "", "", Attribution{}, false)
	attributeCase(t, "pg_sage agent:"+idA+":5", agentguard.LoginRoleName(idA), "",
		Attribution{By: ByApplication, Principal: idA, ActionID: 5}, true)
}

// Invalid input: a malformed agent application name is not an agent claim
// at all; only the role decides.
func TestAttributeMalformedAgentApp(t *testing.T) {
	for _, app := range []string{"pg_sage agent:", "pg_sage agent:Bad Name:1",
		"pg_sage agent:ci-bot:notanumber", "pg_sage agent:ci-bot:1234567890123456789",
		" pg_sage agent:ci-bot:1"} {
		attributeCase(t, app, "app_user", ownRole, Attribution{}, false)
		attributeCase(t, app, "sage_agent_k2m4q7x7ab", ownRole,
			Attribution{By: ByRole, Role: "sage_agent_k2m4q7x7ab"}, true)
	}
}

// An unverified record keeps the claimed name in application, with no
// principal and no action, so nothing joins it to an action's evidence.
func TestRowOfUnverifiedRecordNamesNoPrincipalOrAction(t *testing.T) {
	c := &Correlator{database: "orders"}
	e := entry("pg_sage agent:ci-bot:77", "app_user", ddl)
	rec, ok := Parse(e.Message)
	if !ok {
		t.Fatalf("fixture not parsed")
	}
	a, _ := Attribute(e, ownRole, nil)
	ev := c.row(e, rec, a)
	if ev.PrincipalID != nil || ev.ActionID != nil {
		t.Fatalf("unverified row names principal %v action %v", ev.PrincipalID, ev.ActionID)
	}
	if ev.CorrelatedBy != ByApplicationUnverified || ev.Application != e.Application ||
		ev.DBUser != "app_user" || ev.DatabaseName != "orders" ||
		ev.Command != "CREATE TABLE" {
		t.Fatalf("unverified row = %+v", ev)
	}
	verified := c.row(entry("pg_sage agent:ci-bot:77", "sage_agent_k2m4q7x7ab", ddl), rec,
		Attribution{By: ByApplication, Principal: "ci-bot", ActionID: 77})
	if verified.PrincipalID == nil || *verified.PrincipalID != "ci-bot" ||
		verified.ActionID == nil || *verified.ActionID != 77 {
		t.Fatalf("verified row = %+v", verified)
	}
}
