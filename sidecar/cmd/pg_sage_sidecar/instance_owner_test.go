package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// advisors-08: databases of one PostgreSQL instance elect one owner for
// the instance-level advice (WAL, connections, ALTER SYSTEM).

var clusterA = instanceKey{key: "sysid:7301", source: instanceKeySystemID}

func TestInstanceOwner_LowestNameOwns(t *testing.T) {
	r := newInstanceOwnerRegistry()
	beta, _ := r.register(clusterA, "beta", true)
	alpha, _ := r.register(clusterA, "alpha", true)
	if owner, is := r.owner(alpha); owner != "alpha" || !is {
		t.Fatalf("alpha: owner=%q isOwner=%v, want alpha true", owner, is)
	}
	if owner, is := r.owner(beta); owner != "alpha" || is {
		t.Fatalf("beta: owner=%q isOwner=%v, want alpha false", owner, is)
	}
}

// State transition: the owner stops; the other database owns on the next
// call.
func TestInstanceOwner_ReelectedWhenOwnerStops(t *testing.T) {
	r := newInstanceOwnerRegistry()
	ctx, stop := context.WithCancel(context.Background())
	r.join(ctx, clusterA, "alpha", true)
	beta := r.join(context.Background(), clusterA, "beta", true)
	if _, is := r.owner(beta); is {
		t.Fatal("beta owns while alpha runs")
	}
	stop()
	eventually(t, 5*time.Second, "beta takes ownership", func() bool {
		_, is := r.owner(beta)
		return is
	})
	if owner, _ := r.owner(beta); owner != "beta" {
		t.Fatalf("owner after alpha stopped = %q, want beta", owner)
	}
}

func TestInstanceOwner_LeaveIsIdempotent(t *testing.T) {
	r := newInstanceOwnerRegistry()
	_, leave := r.register(clusterA, "alpha", true)
	beta, _ := r.register(clusterA, "beta", true)
	leave()
	leave()
	if owner, is := r.owner(beta); owner != "beta" || !is {
		t.Fatalf("owner=%q isOwner=%v", owner, is)
	}
}

// Concurrency: 8 runtimes register at once and exactly one owns.
func TestInstanceOwner_ConcurrentRegistrationOneOwner(t *testing.T) {
	r := newInstanceOwnerRegistry()
	names := []string{"db7", "db3", "db5", "db0", "db6", "db2", "db4", "db1"}
	members := make([]*instanceMember, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			members[i], _ = r.register(clusterA, name, true)
		}()
	}
	wg.Wait()
	owners := 0
	for i, m := range members {
		owner, is := r.owner(m)
		if owner != "db0" {
			t.Fatalf("%s sees owner %q, want db0", names[i], owner)
		}
		if is {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("owners = %d, want exactly 1", owners)
	}
}

// Nil/empty: an empty identity, or no member, is per-database behaviour.
func TestInstanceOwner_EmptyIdentityIsPerDatabase(t *testing.T) {
	r := newInstanceOwnerRegistry()
	a, _ := r.register(instanceKey{reason: "identity unreadable"}, "alpha", true)
	b, _ := r.register(instanceKey{reason: "identity unreadable"}, "beta", true)
	for _, m := range []*instanceMember{a, b, nil} {
		if owner, is := r.owner(m); owner != "" || !is {
			t.Fatalf("owner=%q isOwner=%v, want per-database", owner, is)
		}
	}
	got := r.ownership(b)
	if !got.IsOwner || got.Owner != "" || got.Reason != "identity unreadable" {
		t.Fatalf("ownership = %+v", got)
	}
}

// Boundary: two databases on different clusters each own their instance.
func TestInstanceOwner_DifferentClustersTwoOwners(t *testing.T) {
	r := newInstanceOwnerRegistry()
	a, _ := r.register(clusterA, "alpha", true)
	b, _ := r.register(instanceKey{key: "sysid:9999", source: instanceKeySystemID},
		"beta", true)
	for _, m := range []*instanceMember{a, b} {
		if owner, is := r.owner(m); !is || owner != m.database {
			t.Fatalf("%s: owner=%q isOwner=%v", m.database, owner, is)
		}
	}
}

// A database whose advisor is off never owns while one with it on runs,
// so the instance question is not left unasked.
func TestInstanceOwner_CandidatesOwnFirst(t *testing.T) {
	r := newInstanceOwnerRegistry()
	alpha, _ := r.register(clusterA, "alpha", false)
	beta, _ := r.register(clusterA, "beta", true)
	if owner, is := r.owner(alpha); owner != "beta" || is {
		t.Fatalf("alpha (advisor off): owner=%q isOwner=%v, want beta false", owner, is)
	}
	if owner, is := r.owner(beta); owner != "beta" || !is {
		t.Fatalf("beta: owner=%q isOwner=%v", owner, is)
	}
	r2 := newInstanceOwnerRegistry()
	x, _ := r2.register(clusterA, "x", false)
	y, _ := r2.register(clusterA, "y", false)
	if owner, is := r2.owner(x); owner != "x" || !is {
		t.Fatalf("no candidates: x owner=%q isOwner=%v, want the lowest name", owner, is)
	}
	if _, is := r2.owner(y); is {
		t.Fatal("no candidates: y owns too")
	}
}

func TestInstanceOwner_OwnershipReport(t *testing.T) {
	r := newInstanceOwnerRegistry()
	r.register(clusterA, "alpha", true)
	beta, _ := r.register(clusterA, "beta", true)
	got := r.ownership(beta)
	if got.Owner != "alpha" || got.IsOwner || got.KeySource != instanceKeySystemID ||
		got.Reason != "" {
		t.Fatalf("ownership = %+v", got)
	}
}

// The key prefers the system identifier, then the address. A
// pg_control_system permission error leaves the system identifier empty,
// so the address is used; a provider ref alone never keys the instance.
func TestInstanceKeyOf_FallbackOrder(t *testing.T) {
	addr := factClusterAddressKey("db.internal", 5432)
	for _, tc := range []struct {
		name string
		id   envbind.Identity
		want instanceKey
	}{
		{"system identifier", envbind.Identity{SystemIdentifier: " 7301 ", ProviderRef: "rds:a"},
			instanceKey{key: "sysid:7301", source: instanceKeySystemID}},
		{"permission denied: address", envbind.Identity{ProviderRef: "rds:db-1"},
			instanceKey{key: addr, source: instanceKeyAddress}},
		{"address", envbind.Identity{},
			instanceKey{key: addr, source: instanceKeyAddress}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := instanceKeyOf(tc.id, addr); got != tc.want {
				t.Fatalf("key = %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := instanceKeyOf(envbind.Identity{}, ""); got.key != "" || got.reason == "" {
		t.Fatalf("no identity at all: %+v, want an empty key with a reason", got)
	}
}

// Boundary: a physical standby shares its primary's system identifier but
// has its own settings, so it owns its own instance, and two standbys of
// one primary at different addresses do too.
func TestInstanceKeyOf_StandbyIsItsOwnInstance(t *testing.T) {
	addrP := factClusterAddressKey("primary.internal", 5432)
	addrS1 := factClusterAddressKey("standby1.internal", 5432)
	addrS2 := factClusterAddressKey("standby2.internal", 5432)
	primary := instanceKeyOf(envbind.Identity{SystemIdentifier: "7301"}, addrP)
	s1 := instanceKeyOf(envbind.Identity{SystemIdentifier: "7301", InRecovery: true}, addrS1)
	s1Again := instanceKeyOf(envbind.Identity{SystemIdentifier: "7301", InRecovery: true},
		addrS1)
	s2 := instanceKeyOf(envbind.Identity{SystemIdentifier: "7301", InRecovery: true}, addrS2)
	if primary.key == s1.key || s1.key == s2.key || primary.key == s2.key {
		t.Fatalf("keys collide: primary %q standby1 %q standby2 %q", primary.key, s1.key,
			s2.key)
	}
	if s1 != s1Again || s1.source != instanceKeySystemID {
		t.Fatalf("one standby's databases key differently: %+v vs %+v", s1, s1Again)
	}
	r := newInstanceOwnerRegistry()
	p, _ := r.register(primary, "a_primary", true)
	sb, _ := r.register(s1, "b_standby", true)
	for _, m := range []*instanceMember{p, sb} {
		if owner, is := r.owner(m); !is || owner != m.database {
			t.Fatalf("%s: owner=%q isOwner=%v, want its own instance", m.database, owner, is)
		}
	}
}

// Error propagation: an unreachable server gives an empty key with the
// error as the reason (per-database behaviour, reported).
func TestReadInstanceKey_NoPool(t *testing.T) {
	got := readInstanceKey(context.Background(), nil)
	if got.key != "" || got.reason == "" {
		t.Fatalf("key = %+v, want empty with a reason", got)
	}
}
