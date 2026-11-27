package runway

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Fleet cluster ownership (forecast-09). With N databases of one
// PostgreSQL cluster in one fleet process, every runtime used to report
// the cluster's series (xid and mxid age, disk use, every replication
// slot) into its own database: N findings, N cases, N investigations and
// N alerts for one filling slot. A ClusterOwners, one per process, elects
// per cluster the one runtime that reports them. Every runtime still
// samples them, so a new owner has warm history.

// OwnerKey names the cluster a wal_runway reading came from for
// ownership: its system identifier and postmaster start (a standby or a
// restored clone shares the identifier, never the start). The role is
// not part of it: every database's runtime sees the same slots and
// counters whatever role it connects as. Without a system identifier
// there is no key and the runtime reports on its own.
func OwnerKey(w probes.WALRunway) string {
	if strings.TrimSpace(w.SystemID) == "" {
		return ""
	}
	return w.SystemID + "/" + w.StartedAt.UTC().Format(time.RFC3339Nano)
}

// ClusterOwners elects, per cluster key, the runtime that reports the
// cluster-scope series: the lowest database name among the runtimes whose
// last claim is younger than their ttl. It is safe for concurrent use.
type ClusterOwners struct {
	now      func() time.Time
	mu       sync.Mutex
	clusters map[string]*ownerCluster
}

// ownerCluster is one cluster's live members and the owner's last
// successful evaluation.
type ownerCluster struct {
	members     map[string]ownerBeat // database -> last claim
	evaluatedBy string
	evaluatedAt time.Time
}

// ownerBeat is one member's last claim and how long it stays live.
type ownerBeat struct {
	at  time.Time
	ttl time.Duration
}

// NewClusterOwners returns an empty set.
func NewClusterOwners() *ClusterOwners {
	return &ClusterOwners{now: time.Now, clusters: map[string]*ownerCluster{}}
}

// Claim records that database is live on key until ttl passes (a missed
// heartbeat of 2x its interval re-elects) and returns key's owner; ok
// reports that database is it. An empty key or a nil set is
// per-database: (database, true).
func (o *ClusterOwners) Claim(key, database string, ttl time.Duration) (string, bool) {
	if o == nil || key == "" {
		return database, true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.clusters[key]
	if c == nil {
		c = &ownerCluster{members: map[string]ownerBeat{}}
		o.clusters[key] = c
	}
	o.ownerLocked(c) // a claim past its own ttl rejoins without its record
	c.members[database] = ownerBeat{at: o.now(), ttl: ttl}
	owner := o.ownerLocked(c)
	return owner, owner == database
}

// ownerLocked drops c's expired members and returns the lowest live name.
func (o *ClusterOwners) ownerLocked(c *ownerCluster) string {
	now := o.now()
	owner := ""
	for name, b := range c.members {
		if now.Sub(b.at) > b.ttl {
			c.drop(name)
			continue
		}
		if owner == "" || name < owner {
			owner = name
		}
	}
	return owner
}

// drop removes a member and its evaluation record: an owner that left or
// expired and comes back counts only once it has evaluated again.
func (c *ownerCluster) drop(name string) {
	delete(c.members, name)
	if c.evaluatedBy == name {
		c.evaluatedBy, c.evaluatedAt = "", time.Time{}
	}
}

// Leave removes database from key (its runtime ended, or its cluster
// changed), so the next claim on key re-elects. Leaving twice is safe.
func (o *ClusterOwners) Leave(key, database string) {
	if o == nil || key == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.clusters[key]
	if c == nil {
		return
	}
	c.drop(database)
	if len(c.members) == 0 {
		delete(o.clusters, key)
	}
}

// Members lists key's live databases, sorted.
func (o *ClusterOwners) Members(key string) []string {
	if o == nil || key == "" {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.clusters[key]
	if c == nil {
		return nil
	}
	o.ownerLocked(c)
	names := make([]string, 0, len(c.members))
	for name := range c.members {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Evaluated records that database, key's current owner, evaluated and
// reported the cluster series successfully now. A database that is not
// the owner records nothing.
func (o *ClusterOwners) Evaluated(key, database string) {
	if o == nil || key == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.clusters[key]
	if c == nil || o.ownerLocked(c) != database {
		return
	}
	c.evaluatedBy, c.evaluatedAt = database, o.now()
}

// OwnerEvaluated reports whether owner, still key's owner, recorded a
// successful evaluation within the last window. Until it has, a
// non-owner keeps its own open cluster findings: an unhealthy owner
// never makes the cluster's runway disappear.
func (o *ClusterOwners) OwnerEvaluated(key, owner string, window time.Duration) bool {
	if o == nil || key == "" || owner == "" {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.clusters[key]
	if c == nil || o.ownerLocked(c) != owner || c.evaluatedBy != owner {
		return false
	}
	return o.now().Sub(c.evaluatedAt) <= window
}

// ClusterMember is one runtime's fixed membership of a cluster key, for a
// component that does not read the key itself (the forecaster). A nil
// member, or one without a key, is per-database: it owns.
type ClusterMember struct {
	set      *ClusterOwners
	key      string
	database string
	ttl      time.Duration
}

// Member is database's membership of key, live for ttl after each claim.
func (o *ClusterOwners) Member(key, database string, ttl time.Duration) *ClusterMember {
	return &ClusterMember{set: o, key: key, database: database, ttl: ttl}
}

// IsClusterOwner heartbeats the member and returns its cluster's owner;
// ok reports that the member is it.
func (c *ClusterMember) IsClusterOwner() (string, bool) {
	if c == nil {
		return "", true
	}
	return c.set.Claim(c.key, c.database, c.ttl)
}

// Evaluated records the member's successful evaluation as the owner.
func (c *ClusterMember) Evaluated() {
	if c != nil {
		c.set.Evaluated(c.key, c.database)
	}
}

// OwnerHealthy reports that owner evaluated successfully within the ttl.
func (c *ClusterMember) OwnerHealthy(owner string) bool {
	return c != nil && c.set.OwnerEvaluated(c.key, owner, c.ttl)
}

// Databases lists the cluster's live member databases.
func (c *ClusterMember) Databases() []string {
	if c == nil {
		return nil
	}
	return c.set.Members(c.key)
}

// Leave removes the member (its runtime ended).
func (c *ClusterMember) Leave() {
	if c != nil {
		c.set.Leave(c.key, c.database)
	}
}
