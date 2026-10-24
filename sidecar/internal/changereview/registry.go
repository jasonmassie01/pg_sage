package changereview

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Registry maps each monitored database to its Change Review service
// (like ask.Registry). Each database runtime registers its service when
// it starts and removes it when it stops; the API, MCP and the receipts
// job route through here.
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

// Service is the service of database, or ErrNotFound.
func (r *Registry) Service(database string) (*Service, error) {
	s, ok := r.Get(database)
	if !ok {
		return nil, fmt.Errorf("%w: Change Review is not running for database %q",
			ErrNotFound, database)
	}
	return s, nil
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

// Summary is the summary of database since, or with database "" of every
// registered database merged; a database whose read fails is listed in
// Unavailable and the summary is partial, not failed.
func (r *Registry) Summary(ctx context.Context, database string, since time.Time) (Summary,
	error) {
	if database != "" {
		s, err := r.Service(database)
		if err != nil {
			return Summary{}, err
		}
		one, err := s.Summary(ctx, since)
		if err != nil {
			return Summary{}, err
		}
		return MergeSummaries([]Summary{one}, nil), nil
	}
	var parts []Summary
	var unavailable []string
	for _, name := range r.Databases() {
		s, ok := r.Get(name)
		if !ok {
			continue
		}
		one, err := s.Summary(ctx, since)
		switch {
		case errors.Is(err, ErrInvalid):
			return Summary{}, err
		case err != nil:
			s.d.Logf("change review: summary of %s unavailable: %v", name, err)
			unavailable = append(unavailable, name)
			continue
		}
		parts = append(parts, one)
	}
	return MergeSummaries(parts, unavailable), nil
}
