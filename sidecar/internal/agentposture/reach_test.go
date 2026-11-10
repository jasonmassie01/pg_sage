package agentposture

import (
	"slices"
	"strings"
	"testing"
)

// No concurrent access tests: the blanket-revoke helpers are pure
// functions of a read-only Env.

// A blanket REVOKE names the exposed roles, then every inherited group,
// never PUBLIC (callers add it) and never an exposed role twice.
func TestRevokeAllTargets(t *testing.T) {
	if got := roleNames(groupEnv().revokeAllTargets()); !slices.Equal(got,
		[]string{"anon", "authenticated", "readers", "base", "writers"}) {
		t.Fatalf("revokeAllTargets = %v, want exposed then groups in OID order", got)
	}
	// authenticated inherits anon: anon is an exposed role, not a group here.
	via, _ := buildVia([]Role{viaAnon, viaAuthn},
		[]membership{edge(11, "authenticated", 10, "anon")}, maxViaDepth)
	env := Env{Exposed: []Role{viaPublic, viaAnon, viaAuthn}, Via: via}
	if got := roleNames(env.revokeAllTargets()); !slices.Equal(got,
		[]string{"anon", "authenticated"}) {
		t.Fatalf("exposed role as group: %v, want each exposed role once", got)
	}
	if len(env.inheritedGroups()) != 0 || env.inheritedCaveat() != "" {
		t.Fatalf("no group besides exposed roles: groups %v caveat %q",
			env.inheritedGroups(), env.inheritedCaveat())
	}
	if got := (Env{}).revokeAllTargets(); len(got) != 0 {
		t.Fatalf("zero Env: %v", got)
	}
	if got := (Env{Exposed: []Role{viaPublic}}).revokeAllTargets(); len(got) != 0 {
		t.Fatalf("PUBLIC only: %v", got)
	}
	if !strings.Contains(groupEnv().inheritedCaveat(), "every member") {
		t.Fatalf("caveat with groups = %q", groupEnv().inheritedCaveat())
	}
}

// The PostgreSQL 14 AP-06 fixes revoke from the inherited groups too, and
// warn that group members lose the view.
func TestAP06PG14FixesNameGroups(t *testing.T) {
	in := Input{Env: groupEnv(), active: map[string]bool{armNoSecurityInvoker: true}}
	one := ap06Finding(in, "api", "v", []string{"api.t"})
	for _, want := range []string{"REVOKE ALL ON api.v FROM PUBLIC;",
		"REVOKE ALL ON api.v FROM anon;", "REVOKE ALL ON api.v FROM readers;",
		"REVOKE ALL ON api.v FROM base;", "REVOKE ALL ON api.v FROM writers;"} {
		requireContains(t, "AP-06 PG14 fix", one.FixScript, want)
	}
	requireContains(t, "AP-06 PG14 caveat", one.Caveat, "every member")
	schema := ap06SchemaFinding(in, "api", []ap06Item{{schema: "api", name: "v",
		bases: []string{"api.t"}}})
	requireContains(t, "AP-06 PG14 schema fix", schema.FixScript,
		"and the group roles they inherit", "REVOKE ALL ON %s FROM readers",
		"REVOKE ALL ON %s FROM writers")
	requireContains(t, "AP-06 PG14 schema caveat", schema.Caveat, "lose access",
		"every member")
	plain := Input{Env: Env{Exposed: []Role{viaPublic, viaAnon}},
		active: map[string]bool{armNoSecurityInvoker: true}}
	if f := ap06Finding(plain, "api", "v", nil); f.Caveat != "" ||
		strings.Count(f.FixScript, "REVOKE") != 2 {
		t.Fatalf("without groups: caveat %q fix %q", f.Caveat, f.FixScript)
	}
}

// The membership alternative warns that other routes to the group must
// end too: Via keeps only the shortest one.
func TestMembershipFixNamesOtherRoutes(t *testing.T) {
	fix := membershipFix([]Path{{Exposed: 10, Names: []string{"anon", "readers", "base"}}})
	if len(fix) != 1 {
		t.Fatalf("membershipFix = %v, want one line", fix)
	}
	requireContains(t, "membership fix", fix[0], "every other membership leading to base",
		"GRANTED BY", "REVOKE readers FROM anon;")
	if !strings.HasPrefix(fix[0], "-- ") {
		t.Fatalf("the alternative must stay a comment: %q", fix[0])
	}
}
