package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// In fleet mode every database runtime has its own advisor and tuning
// agent, but the WAL and connection questions and every ALTER SYSTEM or
// managed parameter change concern the whole PostgreSQL instance
// (advisors-08). The process elects, per instance, one owner database
// that asks and proposes them; the others defer to it. Databases watched
// by separate sidecar processes are not coordinated.

// instanceOwnerIdentityTimeout bounds the one identity read per runtime.
const instanceOwnerIdentityTimeout = 5 * time.Second

// How an instance was identified, strongest first.
const (
	instanceKeySystemID = "system_identifier"
	instanceKeyAddress  = "address"
)

// instanceKey names a runtime's instance. An empty key (reason says why)
// advises the database on its own.
type instanceKey struct{ key, source, reason string }

// instanceKeyOf keys an identity: the system identifier (EXECUTE on
// pg_control_system()), else the address. A physical standby shares its
// primary's system identifier but has its own settings (ALTER SYSTEM runs
// there too), so a standby is keyed by identifier and address: it never
// defers to its primary, nor two standbys to each other. A proxy fronting
// several clusters at one address groups them: one owner then asks for
// all of them, which loses advice, never adds a change. The managed-cloud
// resource is not a key: telemetry first names it after the advisor is
// built, so it would key a runtime differently depending on timing.
func instanceKeyOf(id envbind.Identity, addr string) instanceKey {
	if sysid := strings.TrimSpace(id.SystemIdentifier); sysid != "" {
		if id.InRecovery {
			return instanceKey{key: "sysid:" + sysid + "/standby@" + addr,
				source: instanceKeySystemID}
		}
		return instanceKey{key: "sysid:" + sysid, source: instanceKeySystemID}
	}
	if addr != "" {
		return instanceKey{key: addr, source: instanceKeyAddress}
	}
	return instanceKey{reason: "instance identity unknown: no system identifier or address"}
}

// readInstanceKey reads pool's live identity. When it cannot be read the
// key is empty and the reason names the error.
func readInstanceKey(ctx context.Context, pool *pgxpool.Pool) instanceKey {
	if pool == nil {
		return instanceKey{reason: "instance identity unreadable: no connection pool"}
	}
	ctx, cancel := context.WithTimeout(ctx, instanceOwnerIdentityTimeout)
	defer cancel()
	id, err := envbind.ReadIdentity(ctx, pool, "")
	if err != nil {
		return instanceKey{reason: "instance identity unreadable: " + err.Error()}
	}
	cc := pool.Config().ConnConfig
	return instanceKeyOf(id, factClusterAddressKey(cc.Host, cc.Port))
}

// instanceMember is one database runtime in the registry. A candidate's
// advisor is on: the owner is a candidate whenever one is registered.
type instanceMember struct {
	key       instanceKey
	database  string
	candidate bool
}

// instanceOwnerRegistry holds the process's runtimes by instance.
type instanceOwnerRegistry struct {
	mu      sync.RWMutex
	members map[*instanceMember]struct{}
}

func newInstanceOwnerRegistry() *instanceOwnerRegistry {
	return &instanceOwnerRegistry{members: map[*instanceMember]struct{}{}}
}

// processInstanceOwners is shared by every database runtime of the process.
var processInstanceOwners = newInstanceOwnerRegistry()

// register adds a runtime; the returned leave removes it (and is safe to
// call more than once).
func (r *instanceOwnerRegistry) register(key instanceKey, database string,
	candidate bool) (*instanceMember, func()) {
	m := &instanceMember{key: key, database: database, candidate: candidate}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.members[m] = struct{}{}
	return m, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.members, m)
	}
}

// join registers a runtime for the life of ctx: stopping or replacing the
// runtime cancels ctx, which leaves, and the next call re-elects.
func (r *instanceOwnerRegistry) join(ctx context.Context, key instanceKey, database string,
	candidate bool) *instanceMember {
	m, leave := r.register(key, database, candidate)
	context.AfterFunc(ctx, leave)
	return m
}

// owner is the owner of m's instance: the lowest database name among the
// registered candidates of its key, or among all its runtimes when none
// is a candidate. An empty key, or no member, is per-database: ("", true).
func (r *instanceOwnerRegistry) owner(m *instanceMember) (string, bool) {
	if m == nil || m.key.key == "" {
		return "", true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var best, bestCandidate string
	for other := range r.members {
		if other.key.key != m.key.key {
			continue
		}
		if best == "" || other.database < best {
			best = other.database
		}
		if other.candidate && (bestCandidate == "" || other.database < bestCandidate) {
			bestCandidate = other.database
		}
	}
	if bestCandidate != "" {
		best = bestCandidate
	}
	if best == "" { // m has left: advise it on its own
		return "", true
	}
	return best, best == m.database
}

// ownerFunc is m's owner as the advisor and the tuning agent read it.
func (r *instanceOwnerRegistry) ownerFunc(m *instanceMember) advisor.InstanceOwner {
	return func() (string, bool) { return r.owner(m) }
}

// ownership is m's owner for the fleet capability report.
func (r *instanceOwnerRegistry) ownership(m *instanceMember) fleet.InstanceOwnership {
	owner, isOwner := r.owner(m)
	o := fleet.InstanceOwnership{Owner: owner, IsOwner: isOwner}
	if m != nil {
		o.KeySource, o.Reason = m.key.source, m.key.reason
	}
	return o
}

// joinInstanceOwner places a fleet runtime in the process registry before
// its advisor and tuning agent are built. Single-database mode keeps no
// registry: the database is its own instance owner, as before.
func (rt *databaseRuntime) joinInstanceOwner() {
	if rt.spec.Shared {
		return
	}
	key := readInstanceKey(rt.ctx, rt.spec.Pool)
	if key.key == "" {
		logWarn(rt.spec.Scope, "db %q: %s; its instance settings are advised for it "+
			"alone", rt.spec.Name, key.reason)
	} else {
		logInfo(rt.spec.Scope, "db %q: instance identified by %s", rt.spec.Name, key.source)
	}
	rt.instanceOwner = processInstanceOwners.join(rt.ctx, key, rt.spec.Name,
		rt.advisorActive())
}

// instanceOwnerFunc is nil outside fleet mode (the database owns).
func (rt *databaseRuntime) instanceOwnerFunc() advisor.InstanceOwner {
	if rt.instanceOwner == nil {
		return nil
	}
	return processInstanceOwners.ownerFunc(rt.instanceOwner)
}

// instanceOwnership feeds the fleet capability report; nil outside fleet
// mode.
func (rt *databaseRuntime) instanceOwnership() func() fleet.InstanceOwnership {
	if rt.instanceOwner == nil {
		return nil
	}
	m := rt.instanceOwner
	return func() fleet.InstanceOwnership { return processInstanceOwners.ownership(m) }
}
