package main

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/policy"
)

// facts-02: the database runtimes of one process are grouped by cluster,
// so a confirmed slot fact of one database binds the cluster-wide WAL
// bounds proposed for its siblings.

type clusterFacts struct{ list []facts.Fact }

func (c clusterFacts) Confirmed(context.Context) ([]facts.Fact, error) {
	return c.list, nil
}

func slotFact(id int64, subject string) facts.Fact {
	at := time.Now().Add(-time.Hour)
	return facts.Fact{ID: id, Type: facts.TypeSlotConsumer, Kind: facts.KindSlot,
		Subject: subject, Status: facts.StatusConfirmed, DecidedBy: "op", DecidedAt: &at}
}

func siblingNames(dbs []facts.SiblingDatabase) []string {
	out := []string{}
	for _, d := range dbs {
		out = append(out, d.Database)
	}
	return out
}

func TestFactClusterSiblingsShareOnlyTheClusterKey(t *testing.T) {
	r := newFactClusterRegistry()
	a, _ := r.register(factClusterID{sysid: "1"}, "orders_a", clusterFacts{})
	b, _ := r.register(factClusterID{sysid: "1"}, "orders_b", clusterFacts{})
	c, _ := r.register(factClusterID{sysid: "2"}, "billing", clusterFacts{})
	z, _ := r.register(factClusterID{sysid: "1"}, "analytics", clusterFacts{})
	if got := siblingNames(r.siblings(b)); !reflect.DeepEqual(got,
		[]string{"analytics", "orders_a"}) {
		t.Fatalf("siblings of orders_b = %v, want analytics and orders_a (sorted)", got)
	}
	if got := siblingNames(r.siblings(c)); len(got) != 0 {
		t.Fatalf("another cluster's databases are siblings: %v", got)
	}
	if got := siblingNames(r.siblings(a)); !reflect.DeepEqual(got,
		[]string{"analytics", "orders_b"}) {
		t.Fatalf("siblings of orders_a = %v", got)
	}
	if got := r.siblings(nil); got != nil {
		t.Fatalf("a runtime outside the registry has siblings: %v", got)
	}
	_ = z
}

// A replaced runtime generation of the same database is not its own
// sibling, and only the newest generation of a sibling is read.
func TestFactClusterGenerationsOfOneDatabase(t *testing.T) {
	r := newFactClusterRegistry()
	oldGen := clusterFacts{list: []facts.Fact{slotFact(1, "old_*")}}
	newGen := clusterFacts{list: []facts.Fact{slotFact(2, "cdc_*")}}
	a1, _ := r.register(factClusterID{sysid: "1"}, "orders_a", oldGen)
	a2, _ := r.register(factClusterID{sysid: "1"}, "orders_a", newGen)
	b, _ := r.register(factClusterID{sysid: "1"}, "orders_b", clusterFacts{})
	if got := r.siblings(a2); len(got) != 1 || got[0].Database != "orders_b" {
		t.Fatalf("siblings of the new generation = %v", siblingNames(got))
	}
	got := r.siblings(b)
	if len(got) != 1 || got[0].Database != "orders_a" ||
		!reflect.DeepEqual(got[0].Facts, facts.ConfirmedSource(newGen)) {
		t.Fatalf("siblings of orders_b = %+v, want only the newest orders_a", got)
	}
	_ = a1
}

// A runtime whose identity read failed (only its address is known) still
// groups with every runtime at its address, including those whose
// identifier is known; two known identifiers decide alone (review of
// facts-02: the fallback must not split one cluster into two groups).
func TestFactClusterUnknownIdentityGroupsByAddress(t *testing.T) {
	r := newFactClusterRegistry()
	const here, there = "addr:db1:5432", "addr:db2:5432"
	known, _ := r.register(factClusterID{sysid: "1", addr: here}, "orders_a", clusterFacts{})
	unknown, _ := r.register(factClusterID{addr: here}, "orders_b", clusterFacts{})
	other, _ := r.register(factClusterID{sysid: "2", addr: here}, "billing", clusterFacts{})
	far, _ := r.register(factClusterID{addr: there}, "remote", clusterFacts{})
	cases := []struct {
		m    *factClusterMember
		want []string
	}{
		{known, []string{"orders_b"}},
		{unknown, []string{"billing", "orders_a"}},
		{other, []string{"orders_b"}},
		{far, []string{}},
	}
	for _, c := range cases {
		if got := siblingNames(r.siblings(c.m)); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("siblings of %s = %v, want %v", c.m.database, got, c.want)
		}
	}
}

func TestFactClusterUnregisterTransitions(t *testing.T) {
	r := newFactClusterRegistry()
	a, leaveA := r.register(factClusterID{sysid: "1"}, "orders_a", clusterFacts{})
	b, leaveB := r.register(factClusterID{sysid: "1"}, "orders_b", clusterFacts{})
	if len(r.siblings(a)) != 1 {
		t.Fatal("orders_b is not a sibling while registered")
	}
	leaveB()
	leaveB() // a second leave is harmless
	if got := r.siblings(a); len(got) != 0 {
		t.Fatalf("orders_b still a sibling after leaving: %v", siblingNames(got))
	}
	if got := r.siblings(b); got != nil {
		t.Fatalf("a runtime that left still sees siblings: %v", siblingNames(got))
	}
	leaveA()
	if n := r.size(); n != 0 {
		t.Fatalf("registry keeps %d members after every runtime left", n)
	}
}

// A runtime joins for the life of its context: cancelling it (removal or
// replacement) leaves the cluster.
func TestFactClusterJoinLeavesWhenTheRuntimeStops(t *testing.T) {
	r := newFactClusterRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	a := r.join(ctx, factClusterID{sysid: "1"}, "orders_a", clusterFacts{})
	b := r.join(context.Background(), factClusterID{sysid: "1"}, "orders_b", clusterFacts{})
	if len(r.siblings(b)) != 1 || len(r.siblings(a)) != 1 {
		t.Fatal("joined runtimes are not siblings")
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for len(r.siblings(b)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a stopped runtime stayed in the cluster")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFactClusterAddressKeyNormalizes(t *testing.T) {
	cases := []struct {
		host string
		port uint16
		want string
	}{
		{"DB.Example.com", 5432, "addr:db.example.com:5432"},
		{"db.example.com", 0, "addr:db.example.com:5432"},
		{" 10.0.0.5 ", 6432, "addr:10.0.0.5:6432"},
		{"::1", 5433, "addr:[::1]:5433"},
		{"/var/run/postgresql", 5432, "addr:/var/run/postgresql:5432"},
		{"", 0, "addr:localhost:5432"},
	}
	for _, c := range cases {
		if got := factClusterAddressKey(c.host, c.port); got != c.want {
			t.Fatalf("factClusterAddressKey(%q, %d) = %q, want %q", c.host, c.port, got,
				c.want)
		}
	}
}

// When the cluster identity cannot be read, only the pool's address is
// known, so the runtime still groups with its siblings there.
func TestFactClusterKeyFallsBackToTheAddress(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	want := factClusterID{addr: "addr:127.0.0.1:1"}
	if got := factClusterIdentity(ctx, pool); got != want {
		t.Fatalf("factClusterIdentity on an unreachable server = %+v, want %+v", got, want)
	}
	if got := factClusterIdentity(ctx, nil); got != (factClusterID{}) {
		t.Fatalf("factClusterIdentity without a pool = %+v, want zero", got)
	}
}

// The runtime binder reads the registry's siblings: a sibling's confirmed
// slot fact blocks this database's cluster-wide bound.
func TestClusterFactBinderReadsSiblings(t *testing.T) {
	r := newFactClusterRegistry()
	r.register(factClusterID{sysid: "9"}, "orders_a",
		clusterFacts{list: []facts.Fact{slotFact(4, "cdc_*")}})
	b, _ := r.register(factClusterID{sysid: "9"}, "orders_b", clusterFacts{})
	binder := newClusterFactBinder(nil, clusterFacts{}, r, b)
	got, err := binder.Bind(context.Background(), policy.ActionRequest{
		SQL:        "ALTER SYSTEM SET max_slot_wal_keep_size = '1MB'",
		TargetObjs: []string{"slot:old_replica"}})
	if err != nil || len(got) != 1 || got[0].FactID != 4 || got[0].Database != "orders_a" {
		t.Fatalf("bindings %+v %v, want orders_a's fact 4", got, err)
	}
	alone := newClusterFactBinder(nil, clusterFacts{}, r, nil)
	if got, err := alone.Bind(context.Background(), policy.ActionRequest{
		SQL: "ALTER SYSTEM SET max_slot_wal_keep_size = '1MB'"}); err != nil || got != nil {
		t.Fatalf("a runtime outside the registry: %+v %v", got, err)
	}
}

// Runtimes join and leave while the gate binds requests. Run under -race
// where cgo is available; here it checks the registry stays consistent.
func TestFactClusterConcurrentJoinLeaveWhileBinding(t *testing.T) {
	r := newFactClusterRegistry()
	b, _ := r.register(factClusterID{sysid: "1"}, "orders_b", clusterFacts{})
	binder := newClusterFactBinder(nil, clusterFacts{}, r, b)
	req := policy.ActionRequest{SQL: "ALTER SYSTEM SET max_slot_wal_keep_size = '1MB'"}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for w := 0; w < 8; w++ {
		wg.Add(2)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, leave := r.register(factClusterID{sysid: "1"},
					"orders_"+string(rune('c'+w)),
					clusterFacts{list: []facts.Fact{slotFact(int64(w+1), "cdc_*")}})
				leave()
			}
		}(w)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				got, err := binder.Bind(context.Background(), req)
				if err != nil {
					errs <- err.Error()
					return
				}
				for _, g := range got {
					if !strings.HasPrefix(g.Database, "orders_") || g.Database == "orders_b" {
						errs <- "binding from " + g.Database
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if got := r.siblings(b); len(got) != 0 {
		t.Fatalf("siblings left after every joiner left: %v", siblingNames(got))
	}
	if n := r.size(); n != 1 {
		t.Fatalf("registry holds %d members, want only orders_b", n)
	}
}
