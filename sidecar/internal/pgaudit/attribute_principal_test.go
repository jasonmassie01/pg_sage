package pgaudit

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// No concurrent access tests here: Attribute and RoleMap are read-only;
// the role cache's concurrency is covered in roles_test.go.

var (
	idA = "agp_" + strings.Repeat("a", 20)
	idB = "agp_" + strings.Repeat("b", 20)
)

const (
	nameA = "agent-a"
	nameB = "agent-b"
)

func ownerRow(id, name string) roleRow {
	return roleRow{principalID: id, name: name,
		loginRole: agentguard.LoginRoleName(id), brokerRole: agentguard.BrokerRoleName(id)}
}

// twoPrincipals is the control database's view of principals A and B.
func twoPrincipals() RoleMap {
	return newRoleMap([]roleRow{ownerRow(idA, nameA), ownerRow(idB, nameB)})
}

func attributeWith(t *testing.T, app, user string, roles RoleResolver, want Attribution) {
	t.Helper()
	got, ok := Attribute(entry(app, user, ddl), ownRole, roles)
	if !ok || got != want {
		t.Fatalf("app %q user %q: got %+v ok=%v, want %+v", app, user, got, ok, want)
	}
}

// Invalid input (the cross-agent spoof): A's own roles claiming B, by slug
// or by id, are kept unverified and name no principal and no action.
func TestAttributeCrossAgentClaimIsUnverified(t *testing.T) {
	unverified := Attribution{By: ByApplicationUnverified}
	roles := twoPrincipals()
	for _, user := range []string{agentguard.LoginRoleName(idA),
		agentguard.BrokerRoleName(idA)} {
		attributeWith(t, "pg_sage agent:"+nameB+":7", user, roles, unverified)
		attributeWith(t, "pg_sage agent:"+idB+":7", user, roles, unverified)
		// A name the principal does not have is no claim of its own either.
		attributeWith(t, "pg_sage agent:agent-a-old:7", user, roles, unverified)
	}
}

// Happy path: a principal's own id from either of its roles is that
// principal and action; its own slug stores its id, not the slug.
func TestAttributeOwnClaimNamesThePrincipalID(t *testing.T) {
	roles := twoPrincipals()
	attributeWith(t, "pg_sage agent:"+idA+":7", agentguard.BrokerRoleName(idA), roles,
		Attribution{By: ByApplication, Principal: idA, ActionID: 7})
	attributeWith(t, "pg_sage agent:"+idA, agentguard.LoginRoleName(idA), roles,
		Attribution{By: ByApplication, Principal: idA})
	attributeWith(t, "pg_sage agent:"+nameA+":9", agentguard.LoginRoleName(idA), roles,
		Attribution{By: ByApplication, Principal: idA, ActionID: 9})
	attributeWith(t, "pg_sage agent:"+nameB, agentguard.BrokerRoleName(idB), roles,
		Attribution{By: ByApplication, Principal: idB})
}

// Boundary: an id one character off is another principal, so A's role
// cannot claim it.
func TestAttributeIDOneCharacterOffIsUnverified(t *testing.T) {
	user := agentguard.BrokerRoleName(idA)
	for i := len("agp_"); i < len(idA); i++ {
		forged := idA[:i] + "c" + idA[i+1:]
		attributeWith(t, "pg_sage agent:"+forged+":7", user, nil,
			Attribution{By: ByApplicationUnverified})
	}
}

// Boundary: a 63-character slug (the longest name) still resolves; one of
// 64 is not an agent claim at all, so only the role decides.
func TestAttributeSlugLengthBoundary(t *testing.T) {
	long := "a" + strings.Repeat("b", 62)
	idC := "agp_" + strings.Repeat("c", 20)
	roles := newRoleMap([]roleRow{ownerRow(idC, long)})
	user := agentguard.LoginRoleName(idC)
	attributeWith(t, "pg_sage agent:"+long+":3", user, roles,
		Attribution{By: ByApplication, Principal: idC, ActionID: 3})
	attributeWith(t, "pg_sage agent:"+long+"b:3", user, roles,
		Attribution{By: ByRole, Role: user, Principal: idC})
}

// Nil/empty: with no resolver (nil, or an empty or nil map) a slug claim is
// unverified and a role names no principal; an id claim still verifies by
// its hash.
func TestAttributeWithoutResolver(t *testing.T) {
	user := agentguard.LoginRoleName(idA)
	for _, roles := range []RoleResolver{nil, RoleMap(nil), RoleMap{}} {
		attributeWith(t, "pg_sage agent:"+nameA+":7", user, roles,
			Attribution{By: ByApplicationUnverified})
		attributeWith(t, "pg_sage agent:"+idA+":7", user, roles,
			Attribution{By: ByApplication, Principal: idA, ActionID: 7})
		attributeWith(t, "psql", user, roles, Attribution{By: ByRole, Role: user})
	}
}

// A role that two principals share (on different clusters) resolves to
// none; the same principal on two clusters is still that principal.
func TestAttributeAmbiguousRoleResolvesToNone(t *testing.T) {
	idC := "agp_" + strings.Repeat("c", 20)
	shared := ownerRow(idC, "agent-c")
	shared.loginRole = agentguard.LoginRoleName(idA)
	roles := newRoleMap([]roleRow{ownerRow(idA, nameA), ownerRow(idA, nameA), shared})
	login, brokerRole := agentguard.LoginRoleName(idA), agentguard.BrokerRoleName(idA)
	attributeWith(t, "pg_sage agent:"+nameA, login, roles,
		Attribution{By: ByApplicationUnverified})
	attributeWith(t, "pg_sage agent:agent-c", login, roles,
		Attribution{By: ByApplicationUnverified})
	attributeWith(t, "psql", login, roles, Attribution{By: ByRole, Role: login})
	attributeWith(t, "pg_sage agent:"+nameA, brokerRole, roles,
		Attribution{By: ByApplication, Principal: idA})
	attributeWith(t, "psql", brokerRole, roles,
		Attribution{By: ByRole, Role: brokerRole, Principal: idA})
}

// A direct session of an agent role carries its principal's id; a role the
// control database does not know names none.
func TestAttributeByRoleCarriesThePrincipal(t *testing.T) {
	roles := twoPrincipals()
	b := agentguard.BrokerRoleName(idB)
	attributeWith(t, "psql", b, roles, Attribution{By: ByRole, Role: b, Principal: idB})
	attributeWith(t, "pg_sage action:5", b, roles,
		Attribution{By: ByRole, Role: b, Principal: idB})
	attributeWith(t, "psql", "sage_agent_k2m4q7x7ab", roles,
		Attribution{By: ByRole, Role: "sage_agent_k2m4q7x7ab"})
}

// Must not change: pg_sage's own sessions and other roles are decided as
// before, whatever the resolver knows.
func TestAttributeOwnSessionsIgnoreTheResolver(t *testing.T) {
	roles := twoPrincipals()
	attributeWith(t, "pg_sage", ownRole, roles, Attribution{By: ByPGSage})
	attributeWith(t, "pg_sage action:77", ownRole, roles,
		Attribution{By: ByActionTag, DecisionID: 77})
	attributeWith(t, "pg_sage agent:"+idA, ownRole, roles,
		Attribution{By: ByApplicationUnverified})
	if _, ok := Attribute(entry("psql", "alice", ddl), ownRole, roles); ok {
		t.Fatalf("another role's session was attributed")
	}
}

// newRoleMap keeps each role's one principal and drops only the roles two
// principals share.
func TestNewRoleMap(t *testing.T) {
	if m := newRoleMap(nil); len(m) != 0 {
		t.Fatalf("no rows: %v", m)
	}
	m := twoPrincipals()
	if len(m) != 4 {
		t.Fatalf("two principals: %d roles, want 4: %v", len(m), m)
	}
	o, ok := m.PrincipalOfRole(agentguard.BrokerRoleName(idA))
	if !ok || o != (Owner{ID: idA, Name: nameA}) {
		t.Fatalf("broker role of A = %+v %v", o, ok)
	}
	if _, ok := m.PrincipalOfRole("sage_agent_aaaaaaaaaa"); ok {
		t.Fatalf("an unknown role resolved")
	}
	// Three principals on one role: still none, not the last one read.
	idC := "agp_" + strings.Repeat("c", 20)
	r1, r2 := ownerRow(idB, nameB), ownerRow(idC, "agent-c")
	r1.loginRole, r2.loginRole = agentguard.LoginRoleName(idA), agentguard.LoginRoleName(idA)
	m = newRoleMap([]roleRow{ownerRow(idA, nameA), r1, r2})
	if _, ok := m.PrincipalOfRole(agentguard.LoginRoleName(idA)); ok {
		t.Fatalf("a role of three principals resolved")
	}
	if o, _ := m.PrincipalOfRole(agentguard.BrokerRoleName(idC)); o.ID != idC {
		t.Fatalf("C's own broker role = %+v", o)
	}
}
