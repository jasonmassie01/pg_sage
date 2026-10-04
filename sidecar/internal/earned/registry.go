package earned

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// RegistryEntry is one database's view of the ledger: the ledger of its
// control database (shared by every database of a meta-database fleet)
// and its own limiter (its HA role, error budget and objects).
type RegistryEntry struct {
	Service *Service
	Limiter *Limiter
}

// Registry maps fleet database names to their ledger, for the API and MCP.
type Registry struct {
	mu       sync.RWMutex
	entries  map[string]RegistryEntry
	enforced bool
}

// NewRegistry reports enforced (sre.autonomy.enforce) with every view.
func NewRegistry(enforced bool) *Registry {
	return &Registry{entries: map[string]RegistryEntry{}, enforced: enforced}
}

// Enforced reports whether the gate consults the ledger.
func (r *Registry) Enforced() bool { return r.enforced }

// Register binds a database; an entry without a ledger is ignored.
func (r *Registry) Register(database string, e RegistryEntry) {
	database = strings.TrimSpace(database)
	if database == "" || e.Service == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[database] = e
}

// Remove unbinds a database (it left the fleet).
func (r *Registry) Remove(database string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, database)
}

// Lookup resolves a database.
func (r *Registry) Lookup(database string) (RegistryEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[database]
	return e, ok
}

// Databases lists the bound databases, sorted.
func (r *Registry) Databases() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for name := range r.entries {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TrustView is a bound database's Trust view with its effective levels;
// false when the database has no ledger (or there is no registry).
func (r *Registry) TrustView(ctx context.Context, database string) (TrustView, bool,
	error) {
	if r == nil {
		return TrustView{}, false, nil
	}
	e, ok := r.Lookup(database)
	if !ok {
		return TrustView{}, false, nil
	}
	v, err := e.Service.TrustView(ctx)
	if err != nil {
		return TrustView{}, true, err
	}
	if e.Limiter != nil {
		e.Limiter.AnnotateTrust(ctx, &v)
	}
	return v, true, nil
}
