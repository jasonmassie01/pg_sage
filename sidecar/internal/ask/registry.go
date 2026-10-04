package ask

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Registry maps each monitored database to its Ask Sage service. Each
// database runtime registers its service when it starts and removes it
// when it stops; the API and MCP route every question through here.
type Registry struct {
	mu       sync.RWMutex
	services map[string]*Service
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{services: map[string]*Service{}} }

// Set registers s for database name, replacing a previous runtime's.
func (r *Registry) Set(name string, s *Service) {
	if r == nil || name == "" || s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services[name] = s
}

// Remove unregisters s for name, unless a newer runtime replaced it.
func (r *Registry) Remove(name string, s *Service) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.services[name] == s {
		delete(r.services, name)
	}
}

// Get is the service of database name.
func (r *Registry) Get(name string) (*Service, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.services[name]
	return s, ok
}

// Databases lists the registered databases, sorted.
func (r *Registry) Databases() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.services))
	for name := range r.services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// AskSage asks database's service; an unknown database is ErrNotFound.
func (r *Registry) AskSage(ctx context.Context, database string, c Caller,
	req Request) (Answer, error) {
	s, ok := r.Get(database)
	if !ok {
		return Answer{}, fmt.Errorf("%w: Ask Sage is not running for database %q",
			ErrNotFound, database)
	}
	return s.Ask(ctx, c, req)
}
