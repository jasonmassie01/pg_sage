package main

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/facts"
)

// Slot facts are cluster facts (facts-02). Replication slots and
// max_slot_wal_keep_size belong to the cluster, but each database keeps
// its facts in its own sage schema. The process groups its database
// runtimes by cluster, so a slot fact confirmed on one database binds the
// slot work and WAL bounds proposed for its siblings. Databases monitored
// by separate sidecar processes do not see each other's facts.

// factClusterIdentityTimeout bounds the one identity read at runtime start.
const factClusterIdentityTimeout = 5 * time.Second

// clusterIdentitySQL reads the cluster's system identifier, which every
// database of one cluster shares (as internal/ha reads it).
const clusterIdentitySQL = `/* pg_sage */ SELECT c.system_identifier::text
FROM pg_catalog.pg_control_system() c`

// factClusterID names a runtime's cluster: the system identifier when it
// could be read (sysid, empty otherwise) and always the pool's address.
type factClusterID struct{ sysid, addr string }

// sameCluster reports whether two runtimes may share a cluster. Two known
// identifiers decide alone; when either is unknown the addresses decide, so
// a runtime whose identity read failed still groups with its siblings at the
// same address (fail closed: a proxy fronting several clusters may group
// too much, never too little).
func (a factClusterID) sameCluster(b factClusterID) bool {
	if a.sysid != "" && b.sysid != "" {
		return a.sysid == b.sysid
	}
	return a.addr == b.addr
}

// factClusterMember is one database runtime in the registry.
type factClusterMember struct {
	id       factClusterID
	database string
	source   facts.ConfirmedSource
	seq      uint64 // registration order: the newest generation wins
}

// factClusterRegistry holds the process's runtimes and their clusters.
type factClusterRegistry struct {
	mu      sync.RWMutex
	seq     uint64
	members map[*factClusterMember]struct{}
}

func newFactClusterRegistry() *factClusterRegistry {
	return &factClusterRegistry{members: map[*factClusterMember]struct{}{}}
}

// processFactClusters is shared by every database runtime of the process.
var processFactClusters = newFactClusterRegistry()

// register adds a runtime of cluster id; the returned leave removes it
// (and is safe to call more than once).
func (r *factClusterRegistry) register(id factClusterID, database string,
	source facts.ConfirmedSource) (*factClusterMember, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	m := &factClusterMember{id: id, database: database, source: source, seq: r.seq}
	r.members[m] = struct{}{}
	return m, func() { r.leave(m) }
}

func (r *factClusterRegistry) leave(m *factClusterMember) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.members, m)
}

// join registers a runtime for the life of ctx: removing or replacing the
// runtime cancels ctx, which leaves the cluster.
func (r *factClusterRegistry) join(ctx context.Context, id factClusterID, database string,
	source facts.ConfirmedSource) *factClusterMember {
	m, leave := r.register(id, database, source)
	context.AfterFunc(ctx, leave)
	return m
}

// siblings are the other databases registered on m's cluster, one entry
// (the newest generation) per database name, sorted by name. m's own
// database, in any generation, is not its sibling.
func (r *factClusterRegistry) siblings(m *factClusterMember) []facts.SiblingDatabase {
	if m == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.members[m]; !ok {
		return nil
	}
	newest := map[string]*factClusterMember{}
	for other := range r.members {
		if other.database == m.database || !m.id.sameCluster(other.id) {
			continue
		}
		if cur, ok := newest[other.database]; !ok || other.seq > cur.seq {
			newest[other.database] = other
		}
	}
	out := make([]facts.SiblingDatabase, 0, len(newest))
	for name, other := range newest {
		out = append(out, facts.SiblingDatabase{Database: name, Facts: other.source})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Database < out[j].Database })
	return out
}

// size is the number of registered runtimes.
func (r *factClusterRegistry) size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.members)
}

// factClusterIdentity names the pool's cluster: its system identifier
// (empty when it cannot be read: some managed services restrict
// pg_control_system, or the server was unreachable) and the pool's
// normalized address. The zero ID without a pool.
func factClusterIdentity(ctx context.Context, pool *pgxpool.Pool) factClusterID {
	if pool == nil {
		return factClusterID{}
	}
	cc := pool.Config().ConnConfig
	id := factClusterID{addr: factClusterAddressKey(cc.Host, cc.Port)}
	ctx, cancel := context.WithTimeout(ctx, factClusterIdentityTimeout)
	defer cancel()
	var sysid string
	err := pool.QueryRow(ctx, clusterIdentitySQL).Scan(&sysid)
	if sysid = strings.TrimSpace(sysid); err == nil && sysid != "" {
		id.sysid = sysid
		return id
	}
	logWarn("facts", "database %q: cluster identity unreadable (%v); slot facts are "+
		"shared with every database at %s", cc.Database, err,
		strings.TrimPrefix(id.addr, "addr:"))
	return id
}

// factClusterAddressKey is the fallback key: lower-cased host and port
// (5432 when unset).
func factClusterAddressKey(host string, port uint16) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		h = "localhost"
	}
	if port == 0 {
		port = 5432
	}
	if strings.HasPrefix(h, "/") { // a Unix socket directory
		return "addr:" + h + ":" + strconv.Itoa(int(port))
	}
	return "addr:" + net.JoinHostPort(h, strconv.Itoa(int(port)))
}

// newClusterFactBinder is a database's gate binder: its own confirmed
// facts, resolved against its catalog, plus its siblings' slot facts.
func newClusterFactBinder(pool *pgxpool.Pool, source facts.ConfirmedSource,
	r *factClusterRegistry, m *factClusterMember) *facts.Binder {
	return facts.NewBinder(source, catalogResolver(pool), nil).WithSiblings(
		func() []facts.SiblingDatabase { return r.siblings(m) })
}

// catalogResolver resolves fact refs against the database's catalog
// (index tables, partition roots); nil without a pool.
func catalogResolver(pool *pgxpool.Pool) facts.RefResolver {
	if pool == nil {
		return nil
	}
	return facts.NewCatalogResolver(pool)
}
