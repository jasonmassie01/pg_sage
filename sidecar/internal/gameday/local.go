package gameday

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/clone"
)

// LocalProvider is the development fallback when no clone provider is
// configured: a disposable database the operator names. It is never a
// monitored database, and Destroy leaves it in place (the fault programs
// clean up after themselves).
type LocalProvider struct{ dsn string }

var _ clone.Provider = (*LocalProvider)(nil)

// NewLocalProvider refuses a DSN that points at any monitored database
// (same host, port and database name).
func NewLocalProvider(dsn string, monitored []string) (*LocalProvider, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, ErrNotConfigured
	}
	target, err := endpointOf(dsn)
	if err != nil {
		return nil, fmt.Errorf("game day local_dsn: %w", err)
	}
	for _, m := range monitored {
		if e, err := endpointOf(m); err == nil && e == target {
			return nil, fmt.Errorf("%w: %s", ErrMonitoredDSN, target)
		}
	}
	return &LocalProvider{dsn: dsn}, nil
}

// Create hands out the local database.
func (p *LocalProvider) Create(context.Context, clone.CloneSpec) (clone.Clone, error) {
	return clone.Clone{DSN: p.dsn, ID: "local", CreatedFrom: time.Now().UTC()}, nil
}

// Destroy leaves the local database in place.
func (p *LocalProvider) Destroy(context.Context, clone.Clone) error { return nil }

// SnapshotAge is zero: the local database is not a snapshot.
func (p *LocalProvider) SnapshotAge(context.Context) (time.Duration, error) { return 0, nil }

// endpointOf is host:port/database of a DSN, lower-cased host.
func endpointOf(dsn string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", fmt.Errorf("not a PostgreSQL DSN")
	}
	return fmt.Sprintf("%s:%d/%s", strings.ToLower(cfg.Host), cfg.Port, cfg.Database), nil
}

// Registry maps fleet database names to their game-day runners.
type Registry struct {
	mu      sync.RWMutex
	runners map[string]*Runner
}

// NewRegistry is an empty registry.
func NewRegistry() *Registry { return &Registry{runners: map[string]*Runner{}} }

// Register binds a database's runner; nil is ignored.
func (r *Registry) Register(database string, runner *Runner) {
	if runner == nil || strings.TrimSpace(database) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runners[database] = runner
}

// Remove unbinds a database.
func (r *Registry) Remove(database string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runners, database)
}

// Lookup resolves a database's runner.
func (r *Registry) Lookup(database string) (*Runner, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	runner, ok := r.runners[database]
	return runner, ok
}
