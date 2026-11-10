package agentposture

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// No concurrent access tests: buildVia and the reach helpers are pure
// functions of their arguments, and Env is read-only once resolved.

func edge(member uint32, memberName string, group uint32, groupName string) membership {
	return membership{member: member, memberName: memberName, group: group,
		groupName: groupName}
}

var (
	viaPublic = Role{OID: PublicOID, Name: "PUBLIC", Source: SourcePublic}
	viaAnon   = Role{OID: 10, Name: "anon", Source: SourceConfigured}
	viaAuthn  = Role{OID: 11, Name: "authenticated", Source: SourceConfigured}
)

func requirePaths(t *testing.T, via map[uint32][]Path, group uint32, want ...[]string) {
	t.Helper()
	got := via[group]
	if len(got) != len(want) {
		t.Fatalf("Via[%d] = %+v, want %d paths %v", group, got, len(want), want)
	}
	for i, w := range want {
		if !slices.Equal(got[i].Names, w) {
			t.Fatalf("Via[%d][%d] = %v, want %v", group, i, got[i].Names, w)
		}
	}
}

func TestBuildVia_DirectAndChained(t *testing.T) {
	via, cut := buildVia([]Role{viaPublic, viaAnon}, []membership{
		edge(10, "anon", 20, "g1"), edge(20, "g1", 30, "g2")}, maxViaDepth)
	if cut || len(via) != 2 {
		t.Fatalf("via = %+v cut = %v, want 2 groups uncut", via, cut)
	}
	requirePaths(t, via, 20, []string{"anon", "g1"})
	requirePaths(t, via, 30, []string{"anon", "g1", "g2"})
	if via[30][0].Exposed != viaAnon.OID || via[30][0].String() != "anon → g1 → g2" {
		t.Fatalf("path = %+v %q", via[30][0], via[30][0].String())
	}
}

func TestBuildVia_NilAndEmpty(t *testing.T) {
	for name, tc := range map[string]struct {
		exposed []Role
		edges   []membership
	}{"nil": {}, "only PUBLIC": {exposed: []Role{viaPublic},
		edges: []membership{edge(0, "PUBLIC", 20, "g")}},
		"no edges": {exposed: []Role{viaPublic, viaAnon}},
		"edges of other roles": {exposed: []Role{viaAnon},
			edges: []membership{edge(99, "other", 20, "g")}}} {
		via, cut := buildVia(tc.exposed, tc.edges, maxViaDepth)
		if len(via) != 0 || cut {
			t.Fatalf("%s: via = %+v cut = %v, want empty", name, via, cut)
		}
	}
}

// chain is anon → r1 → ... → rn.
func chain(n int) []membership {
	out := []membership{edge(10, "anon", 1001, "r1")}
	for i := 2; i <= n; i++ {
		out = append(out, edge(uint32(999+i), fmt.Sprintf("r%d", i-1), uint32(1000+i),
			fmt.Sprintf("r%d", i)))
	}
	return out
}

func TestBuildVia_DepthCap(t *testing.T) {
	via, cut := buildVia([]Role{viaAnon}, chain(maxViaDepth), maxViaDepth)
	if cut || len(via) != maxViaDepth || len(via[1000+maxViaDepth][0].Names) != 17 {
		t.Fatalf("%d-level chain: %d groups, cut %v", maxViaDepth, len(via), cut)
	}
	via, cut = buildVia([]Role{viaAnon}, chain(maxViaDepth+4), maxViaDepth)
	if !cut || len(via) != maxViaDepth {
		t.Fatalf("%d-level chain: %d groups, cut %v, want %d and cut", maxViaDepth+4,
			len(via), cut, maxViaDepth)
	}
	if _, deep := via[1000+maxViaDepth+1]; deep {
		t.Fatal("a group past the depth cap is in Via")
	}
	via, cut = buildVia([]Role{viaAnon}, chain(3), 0)
	if !cut || len(via) != 0 {
		t.Fatalf("depth 0: via %v cut %v, want nothing and cut", via, cut)
	}
}

// PostgreSQL refuses membership cycles; the builder still ends on one.
func TestBuildVia_CycleEnds(t *testing.T) {
	via, cut := buildVia([]Role{viaAnon}, []membership{edge(10, "anon", 20, "a"),
		edge(20, "a", 30, "b"), edge(30, "b", 20, "a"), edge(30, "b", 10, "anon")},
		maxViaDepth)
	if cut || len(via) != 2 {
		t.Fatalf("cycle: via %+v cut %v", via, cut)
	}
	requirePaths(t, via, 20, []string{"anon", "a"})
	requirePaths(t, via, 30, []string{"anon", "a", "b"})
}

// The shortest path wins; among equal ones, the group names decide.
func TestBuildVia_ShortestPathAndSharedGroups(t *testing.T) {
	edges := []membership{edge(10, "anon", 22, "zeta"), edge(10, "anon", 21, "alpha"),
		edge(22, "zeta", 30, "d"), edge(21, "alpha", 30, "d"), edge(10, "anon", 40, "far"),
		edge(40, "far", 41, "far2"), edge(41, "far2", 42, "near"), edge(21, "alpha", 42,
			"near"), edge(11, "authenticated", 30, "d")}
	via, _ := buildVia([]Role{viaPublic, viaAnon, viaAuthn}, edges, maxViaDepth)
	requirePaths(t, via, 30, []string{"anon", "alpha", "d"}, []string{"authenticated", "d"})
	requirePaths(t, via, 42, []string{"anon", "alpha", "near"})
}

// An exposed role that inherits another exposed role (authenticator →
// viaAnon) maps that role too.
func TestBuildVia_ExposedRoleAsGroup(t *testing.T) {
	auth := Role{OID: 12, Name: "authenticator", Source: SourceConfigured}
	via, _ := buildVia([]Role{viaAnon, auth}, []membership{edge(12, "authenticator", 10,
		"anon")}, maxViaDepth)
	requirePaths(t, via, 10, []string{"authenticator", "anon"})
	env := Env{Exposed: []Role{viaPublic, viaAnon, auth}, Via: via}
	if got := env.ReachOIDs(); !slices.Equal(got, []uint32{0, 10, 12}) {
		t.Fatalf("ReachOIDs = %v, want no duplicate of anon", got)
	}
}

func groupEnv() Env {
	via, _ := buildVia([]Role{viaAnon, viaAuthn}, []membership{edge(10, "anon", 50, "readers"),
		edge(50, "readers", 60, "base"), edge(11, "authenticated", 70, "writers")},
		maxViaDepth)
	return Env{Exposed: []Role{viaPublic, viaAnon, viaAuthn}, Via: via}
}

func TestReachOIDs(t *testing.T) {
	if got := (Env{Exposed: []Role{viaPublic, viaAnon}}).ReachOIDs(); !slices.Equal(got,
		[]uint32{0, 10}) {
		t.Fatalf("without Via: %v, want the exposed OIDs", got)
	}
	if got := (Env{}).ReachOIDs(); len(got) != 0 {
		t.Fatalf("zero Env: %v", got)
	}
	if got := groupEnv().ReachOIDs(); !slices.Equal(got, []uint32{0, 10, 11, 50, 60, 70}) {
		t.Fatalf("ReachOIDs = %v, want exposed then groups in OID order", got)
	}
}

func TestReachingRoles_ThroughGroups(t *testing.T) {
	env := groupEnv()
	for name, tc := range map[string]struct {
		grantees, usage []uint32
		want            []string
	}{
		"group grant, direct usage": {[]uint32{60}, []uint32{10}, []string{"anon"}},
		"direct grant, group usage": {[]uint32{11}, []uint32{70}, []string{"authenticated"}},
		"group grant, no usage":     {[]uint32{50}, []uint32{11}, nil},
		"other group":               {[]uint32{70}, []uint32{10, 11}, []string{"authenticated"}},
		"PUBLIC grant, PUBLIC usage": {[]uint32{0}, []uint32{0},
			[]string{"PUBLIC", "anon", "authenticated"}},
		"unknown grantee": {[]uint32{999}, []uint32{0}, nil},
		"empty":           {nil, nil, nil},
	} {
		if got := roleNames(reachingRoles(env, tc.grantees, tc.usage)); !slices.Equal(got,
			tc.want) {
			t.Fatalf("%s: reaching = %v, want %v", name, got, tc.want)
		}
	}
	if got := roleNames(exposedHolders(env, []uint32{60})); !slices.Equal(got,
		[]string{"anon"}) {
		t.Fatalf("exposedHolders through base = %v, want anon", got)
	}
	holders := exposedHolders(env, []uint32{10, 11})
	if got := roleNames(reaching(env, holders, []uint32{50})); !slices.Equal(got,
		[]string{"anon"}) {
		t.Fatalf("reaching with usage on readers = %v, want anon", got)
	}
}

func TestRevokeTargets_IncludeGroups(t *testing.T) {
	env := groupEnv()
	reach := []Role{viaAnon, viaAuthn}
	got := roleNames(revokeTargets(env, reach, []uint32{60, 0, 10, 70, 999}))
	want := []string{"PUBLIC", "anon", "base", "writers"}
	if !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	if got := roleNames(revokeTargets(env, []Role{viaAuthn}, []uint32{60})); len(got) != 0 {
		t.Fatalf("base reaches only anon, but targets for authenticated = %v", got)
	}
	if got := revokeTargets(Env{}, nil, nil); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
}

func TestGroupTexts(t *testing.T) {
	env := groupEnv()
	if got := grantText(env, []string{"60:SELECT", "10:UPDATE", "999:DELETE"}); got !=
		"UPDATE to anon; SELECT to base; DELETE to role 999" {
		t.Fatalf("grantText = %q", got)
	}
	ps := env.paths([]Role{viaAnon, viaAuthn}, []uint32{60, 70, 60})
	if len(ps) != 2 || viaText(ps) != " (through anon → readers → base; authenticated → "+
		"writers)" {
		t.Fatalf("paths = %+v, text %q", ps, viaText(ps))
	}
	fix := membershipFix(append(ps, ps...))
	if len(fix) != 2 || !strings.Contains(fix[0], "REVOKE readers FROM anon;") ||
		!strings.Contains(fix[1], "REVOKE writers FROM authenticated;") {
		t.Fatalf("membershipFix = %v", fix)
	}
	if viaText(nil) != "" || groupCaveat(nil) != "" || len(membershipFix(nil)) != 0 {
		t.Fatal("no path must add no text")
	}
	if !strings.Contains(groupCaveat(ps), "every member") {
		t.Fatalf("caveat = %q", groupCaveat(ps))
	}
}

// failQuerier fails every query; queried counts the calls.
type failQuerier struct{ queried int }

var errBoom = errors.New("connection reset by peer")

func (q *failQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.queried++
	return nil, errBoom
}

func (q *failQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

func TestResolveVia_Errors(t *testing.T) {
	q := &failQuerier{}
	env := Env{Exposed: []Role{viaPublic}}
	if err := resolveVia(context.Background(), q, &env); err != nil || q.queried != 0 {
		t.Fatalf("PUBLIC only: err %v after %d queries, want no query", err, q.queried)
	}
	env = Env{VersionNum: 170000, Exposed: []Role{viaPublic, viaAnon}}
	err := resolveVia(context.Background(), q, &env)
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "role memberships") {
		t.Fatalf("err = %v, want the membership read named and the cause wrapped", err)
	}
	if env.Via != nil {
		t.Fatalf("Via = %v after a failed read", env.Via)
	}
}

func TestMembershipSQL_ByVersion(t *testing.T) {
	if !strings.Contains(membershipRoleLevelSQL, "r.rolinherit") ||
		strings.Contains(membershipRoleLevelSQL, "inherit_option") {
		t.Fatal("PostgreSQL 14/15 statement must test the member's rolinherit")
	}
	if !strings.Contains(membershipPerGrantSQL, "m.inherit_option") ||
		strings.Contains(membershipPerGrantSQL, "rolinherit") {
		t.Fatal("PostgreSQL 16+ statement must test each membership's inherit_option")
	}
}
