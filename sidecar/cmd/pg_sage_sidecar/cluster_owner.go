package main

import (
	"net"
	"slices"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/rca"
)

// clusterOwnerEngine is the RCA engine's cluster-ownership switch.
type clusterOwnerEngine interface {
	SetClusterOwner(owner bool)
}

// clusterOwnerSet picks, per PostgreSQL cluster (host:port), the one
// runtime whose RCA engine detects the cluster-wide snapshot signals
// (rca-08): the lowest database name, the rule the log fanout uses for
// cluster log events. It also covers runtimes without a log watcher.
type clusterOwnerSet struct {
	mu       sync.Mutex
	clusters map[string]map[string]clusterOwnerEngine // key -> name -> engine
}

func newClusterOwnerSet() *clusterOwnerSet {
	return &clusterOwnerSet{clusters: map[string]map[string]clusterOwnerEngine{}}
}

// runtimeClusterOwners is the process-wide set every mode uses.
var runtimeClusterOwners = newClusterOwnerSet()

// join adds the engine to its cluster and reassigns ownership. Joining a
// name again replaces the engine registered under it.
func (s *clusterOwnerSet) join(key, name string, eng clusterOwnerEngine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	members := s.clusters[key]
	if members == nil {
		members = map[string]clusterOwnerEngine{}
		s.clusters[key] = members
	}
	members[name] = eng
	assignClusterOwner(members)
}

// leave removes the engine from its cluster, unless a later join replaced
// it, and hands ownership to the next name.
func (s *clusterOwnerSet) leave(key, name string, eng clusterOwnerEngine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	members := s.clusters[key]
	if members == nil || members[name] != eng {
		return
	}
	delete(members, name)
	if len(members) == 0 {
		delete(s.clusters, key)
		return
	}
	assignClusterOwner(members)
}

// assignClusterOwner makes the lowest name the owner and the others not.
func assignClusterOwner(members map[string]clusterOwnerEngine) {
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, name := range names {
		members[name].SetClusterOwner(i == 0)
	}
}

// poolHostPort names the pool's PostgreSQL cluster by host:port; "" when
// there is no pool.
func poolHostPort(pool *pgxpool.Pool) string {
	if pool == nil {
		return ""
	}
	conn := pool.Config().ConnConfig
	return net.JoinHostPort(conn.Host, strconv.Itoa(int(conn.Port)))
}

// joinClusterScope registers the runtime's engine with its cluster and,
// when the runtime ends (a database removed at runtime), leaves it and
// stops the log subscription (nil without one), so its buffer is freed
// and ownership of the cluster's signals moves to the next database.
func (rt *databaseRuntime) joinClusterScope(sub rca.LogSource) {
	key, name, eng := poolHostPort(rt.spec.Pool), rt.spec.Name, rt.rca
	if key != "" {
		runtimeClusterOwners.join(key, name, eng)
	}
	rt.start(func() {
		<-rt.ctx.Done()
		if key != "" {
			runtimeClusterOwners.leave(key, name, eng)
		}
		if sub != nil {
			sub.Stop()
		}
	})
}
