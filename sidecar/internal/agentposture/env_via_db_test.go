package agentposture

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: Env is built once per run and only read.

func roleOID(f *fixture, name string) uint32 {
	f.t.Helper()
	var oid uint32
	if err := f.pool.QueryRow(f.ctx, "SELECT oid FROM pg_roles WHERE rolname = $1",
		name).Scan(&oid); err != nil {
		f.t.Fatalf("role %s: %v", name, err)
	}
	return oid
}

// ResolveEnv maps each inherited group to the exposed role's shortest path.
func TestResolveEnv_ViaGroupPaths(t *testing.T) {
	f := newFixture(t)
	g1 := f.role("pgra_", "NOLOGIN")
	g2 := f.role("pgrb_", "NOLOGIN")
	f.exec("GRANT "+g1+" TO "+f.exposed, "GRANT "+g2+" TO "+g1)
	env := f.env(nil)
	o1, o2, oe := roleOID(f, g1), roleOID(f, g2), roleOID(f, f.exposed)
	want1 := []Path{{Exposed: oe, Names: []string{f.exposed, g1}}}
	want2 := []Path{{Exposed: oe, Names: []string{f.exposed, g1, g2}}}
	if !slices.EqualFunc(env.Via[o1], want1, pathEqual) ||
		!slices.EqualFunc(env.Via[o2], want2, pathEqual) {
		t.Fatalf("Via = %+v, want %s: %v and %s: %v", env.Via, g1, want1, g2, want2)
	}
	reach := env.ReachOIDs()
	if !slices.Equal(reach[:len(env.Exposed)], env.ExposedOIDs()) ||
		!slices.Contains(reach, o1) || !slices.Contains(reach, o2) ||
		len(reach) != len(env.Exposed)+2 {
		t.Fatalf("ReachOIDs = %v, want the exposed %v then %d and %d", reach,
			env.ExposedOIDs(), o1, o2)
	}
	if len(env.ViaTruncated) != 0 {
		t.Fatalf("ViaTruncated = %v on 2 memberships", env.ViaTruncated)
	}
}

func pathEqual(a, b Path) bool {
	return a.Exposed == b.Exposed && slices.Equal(a.Names, b.Names)
}

// With PUBLIC the only exposed role there is no membership to follow.
func TestResolveEnv_NoExposedRolesNoVia(t *testing.T) {
	testdb.HoldSupabaseRoles(t, testdb.SkipUnlessLive(t))
	f := newFixture(t)
	g := f.role("pgrp_", "NOLOGIN")
	f.exec("GRANT " + g + " TO " + f.exposed)
	if env := f.env(nil); len(env.Via) == 0 {
		t.Fatal("control: the fixture's exposed role inherits a group but Via is empty")
	}
	cfg := DefaultConfig()
	cfg.ClientPatterns = nil
	var got Env
	var err error
	readTx(t, f.ctx, f.pool, func(tx pgx.Tx) { got, err = ResolveEnv(f.ctx, tx, cfg) })
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	if len(got.Exposed) != 1 || len(got.Via) != 0 || len(got.ViaTruncated) != 0 ||
		!slices.Equal(got.ReachOIDs(), []uint32{PublicOID}) {
		t.Fatalf("env = exposed %v via %v truncated %v reach %v, want PUBLIC only",
			got.Exposed, got.Via, got.ViaTruncated, got.ReachOIDs())
	}
}

// A membership read past maxMemberships is reported as the truncation of
// every detector that follows membership, so open findings stay open.
func TestResolveEnv_MembershipLimit(t *testing.T) {
	f := newFixture(t)
	g1 := f.role("pgra_", "NOLOGIN")
	g2 := f.role("pgrb_", "NOLOGIN")
	f.exec("GRANT "+g1+" TO "+f.exposed, "GRANT "+g2+" TO "+f.exposed)
	withLimit(t, &maxMemberships, 2)
	if env := f.env(nil); len(env.ViaTruncated) != 0 || len(env.Via) != 2 {
		t.Fatalf("2 memberships at limit 2: via %v truncated %v", env.Via, env.ViaTruncated)
	}
	withLimit(t, &maxMemberships, 1)
	env := f.env(nil)
	want := []Truncation{{Arm: "memberships", Limit: 1}}
	if !slices.Equal(env.ViaTruncated, want) || len(env.Via) != 1 {
		t.Fatalf("2 memberships at limit 1: via %v truncated %v, want %v", env.Via,
			env.ViaTruncated, want)
	}
	for _, id := range []string{"AP-03", "AP-04", "AP-05", "AP-06", "AP-09"} {
		if o := f.run(id, env); !slices.Contains(o.Truncated, want[0]) {
			t.Fatalf("%s truncated = %v, want %v", id, o.Truncated, want[0])
		}
	}
	if o := f.run("AP-15", env); len(o.Truncated) != 0 {
		t.Fatalf("AP-15 does not follow membership but reports %v", o.Truncated)
	}
}
