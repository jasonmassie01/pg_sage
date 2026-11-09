package managedparam

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Peers (clouds-08): the databases a process monitors on one provider
// instance share its parameter group or database flags, so a change one
// of them holds (open, or recently rejected) is not proposed again by the
// others. Each database's proposals stay in its own store, where they are
// approved; a database whose peers cannot be read proposes as before.

// peerReadTimeout bounds one read of a peer's store: a peer that hangs
// must not stall the other members, which wait on the resource's lock.
const peerReadTimeout = 5 * time.Second

// peer is one member database's store.
type peer struct {
	database string
	store    *Store
}

// peerGroup is the member databases of one provider resource. propose
// serializes each member's peer check and upsert, so members cycling at
// the same moment store a change once.
type peerGroup struct {
	propose sync.Mutex
	members map[string]*Store
}

var peerRegistry = struct {
	sync.Mutex
	groups map[string]*peerGroup
}{groups: map[string]*peerGroup{}}

// JoinPeers registers database's store as a member of resource (the
// provider instance's key); leave removes it, unless a newer store has
// replaced it. An empty resource or database, or a nil store, joins
// nothing.
func JoinPeers(resource, database string, store *Store) (leave func()) {
	if resource == "" || database == "" || store == nil {
		return func() {}
	}
	peerRegistry.Lock()
	g := peerRegistry.groups[resource]
	if g == nil {
		g = &peerGroup{members: map[string]*Store{}}
		peerRegistry.groups[resource] = g
	}
	g.members[database] = store
	peerRegistry.Unlock()
	var once sync.Once
	return func() { once.Do(func() { leavePeers(resource, database, store) }) }
}

func leavePeers(resource, database string, store *Store) {
	peerRegistry.Lock()
	defer peerRegistry.Unlock()
	g := peerRegistry.groups[resource]
	if g == nil || g.members[database] != store {
		return
	}
	delete(g.members, database)
	if len(g.members) == 0 {
		delete(peerRegistry.groups, resource)
	}
}

// PeerDatabases returns the sorted member databases of database's
// resource, itself included (nil: it has joined none).
func PeerDatabases(database string) []string {
	peerRegistry.Lock()
	defer peerRegistry.Unlock()
	for _, g := range peerRegistry.groups {
		if _, ok := g.members[database]; !ok {
			continue
		}
		out := make([]string, 0, len(g.members))
		for db := range g.members {
			out = append(out, db)
		}
		sort.Strings(out)
		return out
	}
	return nil
}

func peerGroupFor(resource string) *peerGroup {
	peerRegistry.Lock()
	defer peerRegistry.Unlock()
	return peerRegistry.groups[resource]
}

// otherPeers returns resource's members other than database, sorted.
func otherPeers(resource, database string) []peer {
	peerRegistry.Lock()
	defer peerRegistry.Unlock()
	g := peerRegistry.groups[resource]
	if g == nil {
		return nil
	}
	var out []peer
	for db, s := range g.members {
		if db != database {
			out = append(out, peer{database: db, store: s})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].database < out[j].database })
	return out
}

// holds reports whether the store holds p's change for the same target:
// open, or rejected inside the cooldown.
func (s *Store) holds(ctx context.Context, p Proposal) (bool, error) {
	var held bool
	err := s.pool.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1
		FROM sage.managed_change_proposals
		WHERE provider = $1 AND target = $2 AND parameter = $3 AND value = $4
		  AND (status IN ('pending', 'approved')
		       OR (status = 'rejected'
		           AND decided_at > now() - make_interval(secs => $5))))`,
		p.Provider, p.Target, p.Parameter, p.Value, RejectionCooldown.Seconds()).Scan(&held)
	return held, err
}

// upsert stores p unless a peer database of the same resource holds it
// (held). The peer check and the upsert run under the resource's lock.
func (w *Worker) upsert(ctx context.Context, p Proposal, findingID int64,
	res *CycleResult) (rec Record, created, held bool, err error) {
	if g := peerGroupFor(w.opts.Resource); g != nil {
		g.propose.Lock()
		defer g.propose.Unlock()
		if w.heldByPeer(ctx, p, res) {
			res.HeldByPeer++
			return Record{}, false, true, nil
		}
	}
	rec, created, err = w.store.Upsert(ctx, p, findingID)
	return rec, created, false, err
}

// heldByPeer reports that another member holds p. A database that holds
// its own copy keeps refreshing it; a peer that cannot be read counts as
// not holding it, so a failure never blocks the proposal.
func (w *Worker) heldByPeer(ctx context.Context, p Proposal, res *CycleResult) bool {
	others := otherPeers(w.opts.Resource, w.opts.Database)
	if len(others) == 0 {
		return false
	}
	if own, err := w.store.holds(ctx, p); err != nil || own {
		return false // Upsert refreshes its own copy, or reports the error
	}
	for _, o := range others {
		peerCtx, cancel := context.WithTimeout(ctx, peerReadTimeout)
		held, err := o.store.holds(peerCtx, p)
		cancel()
		res.peerReads++
		if err != nil {
			if res.peerErr == "" {
				res.peerErr = fmt.Sprintf("reading the managed changes of peer database "+
					"%q (same provider instance) failed, so this database is proposing "+
					"them itself and a change may be listed twice until it recovers: %v",
					o.database, err)
			}
			continue
		}
		if held {
			return true
		}
	}
	return false
}

// notePeerError reports a peer read failure once: PeerError is set when
// it first appears or changes, and cleared state is restored once every
// peer read of a cycle succeeds.
func (w *Worker) notePeerError(res *CycleResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case res.peerErr != "" && res.peerErr != w.peerErr:
		res.PeerError, w.peerErr = res.peerErr, res.peerErr
	case res.peerErr == "" && res.peerReads > 0:
		w.peerErr = ""
	}
}
