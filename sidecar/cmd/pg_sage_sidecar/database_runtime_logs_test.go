package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/rca"
)

// rca-08: one RCA engine per PostgreSQL cluster owns the cluster-wide
// signals, and a runtime that ends releases ownership and its log buffer.

func ownerEngine() *rca.Engine {
	return rca.NewEngine(&config.RCAConfig{Enabled: true, ResolutionCycles: 2},
		func(string, string, ...any) {})
}

func owners(engs map[string]*rca.Engine) map[string]bool {
	out := map[string]bool{}
	for name, eng := range engs {
		out[name] = eng.ClusterOwner()
	}
	return out
}

// Happy path and state transitions: the lowest name owns the cluster,
// leaving moves ownership on, and another cluster is independent.
func TestClusterOwnerSetLowestNameOwns(t *testing.T) {
	set := newClusterOwnerSet()
	engs := map[string]*rca.Engine{"c": ownerEngine(), "a": ownerEngine(),
		"b": ownerEngine()}
	for _, name := range []string{"c", "a", "b"} {
		set.join("pg1:5432", name, engs[name])
	}
	other := ownerEngine()
	set.join("pg2:5432", "z", other)
	if got := owners(engs); !got["a"] || got["b"] || got["c"] {
		t.Fatalf("owners = %v, want only a", got)
	}
	set.leave("pg1:5432", "a", engs["a"])
	if got := owners(engs); !got["b"] || got["c"] {
		t.Fatalf("after a left owners = %v, want b", got)
	}
	if !other.ClusterOwner() {
		t.Fatal("the other cluster's only engine lost ownership")
	}
	set.leave("pg1:5432", "b", engs["b"])
	set.leave("pg1:5432", "c", engs["c"])
	if len(set.clusters) != 1 {
		t.Fatalf("clusters = %d after pg1 emptied, want 1", len(set.clusters))
	}
}

// Invalid input: a stale engine leaving (its name was re-joined by a new
// runtime) or an unknown name or cluster changes nothing.
func TestClusterOwnerSetIgnoresStaleLeave(t *testing.T) {
	set := newClusterOwnerSet()
	old, renewed, b := ownerEngine(), ownerEngine(), ownerEngine()
	set.join("k", "a", old)
	set.join("k", "b", b)
	set.join("k", "a", renewed)
	set.leave("k", "a", old)
	set.leave("k", "nope", old)
	set.leave("missing", "a", old)
	if !renewed.ClusterOwner() || b.ClusterOwner() {
		t.Fatalf("owners a=%v b=%v, want the re-joined a to stay owner",
			renewed.ClusterOwner(), b.ClusterOwner())
	}
}

// Concurrency: runtimes join and leave in parallel; the stable lowest
// name ends as the only owner.
func TestClusterOwnerSetConcurrentJoinLeave(t *testing.T) {
	set := newClusterOwnerSet()
	stable := ownerEngine()
	set.join("k", "a", stable)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				eng := ownerEngine()
				set.join("k", fmt.Sprintf("z%d", w), eng)
				if eng.ClusterOwner() {
					t.Errorf("z%d became owner beside a", w)
				}
				set.leave("k", fmt.Sprintf("z%d", w), eng)
			}
		}(w)
	}
	wg.Wait()
	if !stable.ClusterOwner() || len(set.clusters["k"]) != 1 {
		t.Fatalf("owner=%v members=%d, want a alone", stable.ClusterOwner(),
			len(set.clusters["k"]))
	}
}

// Nil/empty: no pool has no cluster key; a pool names host:port.
func TestPoolHostPort(t *testing.T) {
	if got := poolHostPort(nil); got != "" {
		t.Fatalf("poolHostPort(nil) = %q, want empty", got)
	}
	pool := hostPool(t, "postgres://u@db1.internal:6543/app")
	if got := poolHostPort(pool); got != "db1.internal:6543" {
		t.Fatalf("poolHostPort = %q, want db1.internal:6543", got)
	}
}

func hostPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pcfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// State transition: a runtime of the cluster ends (a database removed at
// runtime); it leaves the owner set, ownership moves to the next database
// and its fanout subscription stops, so cluster events reach the new
// owner and nothing buffers for the removed database.
func TestRuntimeEndReleasesClusterScope(t *testing.T) {
	saved := runtimeClusterOwners
	runtimeClusterOwners = newClusterOwnerSet()
	t.Cleanup(func() { runtimeClusterOwners = saved })
	pool := hostPool(t, "postgres://u@pg1:5432/app")
	fanout := logwatch.NewLogFanout(&logwatch.FileWatcher{})
	a, aCancel, aDone := clusterRuntime(t, "a", pool, fanout)
	b, bCancel, bDone := clusterRuntime(t, "b", pool, fanout)
	t.Cleanup(func() { bCancel(); bDone.Wait() })
	if !a.rca.ClusterOwner() || b.rca.ClusterOwner() {
		t.Fatalf("owners a=%v b=%v, want a", a.rca.ClusterOwner(), b.rca.ClusterOwner())
	}
	aCancel()
	aDone.Wait()
	if !b.rca.ClusterOwner() {
		t.Fatal("ownership did not move to b when a's runtime ended")
	}
	if a.sub.stops() != 1 || b.sub.stops() != 0 {
		t.Fatalf("subscription stops a=%d b=%d, want 1/0", a.sub.stops(), b.sub.stops())
	}
	if n := len(runtimeClusterOwners.clusters["pg1:5432"]); n != 1 {
		t.Fatalf("cluster members = %d, want 1", n)
	}
}

type testClusterRuntime struct {
	*databaseRuntime
	sub *stopSpy
}

// stopSpy counts Stop calls on the real fanout subscription it wraps.
type stopSpy struct {
	*logwatch.FanoutSubscriber
	mu sync.Mutex
	n  int
}

func (s *stopSpy) Stop() {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	s.FanoutSubscriber.Stop()
}

func (s *stopSpy) stops() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// A runtime without a log subscription leaves its cluster without
// stopping anything.
func TestRuntimeEndWithoutLogSubscription(t *testing.T) {
	saved := runtimeClusterOwners
	runtimeClusterOwners = newClusterOwnerSet()
	t.Cleanup(func() { runtimeClusterOwners = saved })
	ctx, cancel := context.WithCancel(context.Background())
	rt := &databaseRuntime{ctx: ctx, workers: &sync.WaitGroup{},
		spec: databaseRuntimeSpec{Name: "a", Pool: hostPool(t, "postgres://u@pg1/app")},
		rca:  ownerEngine()}
	rt.joinClusterScope(nil)
	if len(runtimeClusterOwners.clusters["pg1:5432"]) != 1 {
		t.Fatal("runtime without logwatch did not join its cluster")
	}
	cancel()
	rt.workers.Wait()
	if len(runtimeClusterOwners.clusters) != 0 {
		t.Fatal("ended runtime still in its cluster")
	}
}

func clusterRuntime(
	t *testing.T, name string, pool *pgxpool.Pool, fanout *logwatch.LogFanout,
) (testClusterRuntime, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	rt := &databaseRuntime{ctx: ctx, workers: &sync.WaitGroup{},
		spec: databaseRuntimeSpec{Name: name, Pool: pool}, rca: ownerEngine()}
	sub := &stopSpy{FanoutSubscriber: fanout.Subscribe(name)}
	rt.rca.SetLogSource(sub)
	rt.joinClusterScope(sub)
	return testClusterRuntime{rt, sub}, cancel, rt.workers
}
