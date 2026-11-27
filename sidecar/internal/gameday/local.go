package gameday

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/clusterlock"
)

// TargetMarker is the database comment that marks a local game-day
// target as disposable:
//
//	COMMENT ON DATABASE scratch IS 'pg_sage:gameday_target';
const TargetMarker = "pg_sage:gameday_target"

// probeTimeout bounds the target connection and each identity read.
const probeTimeout = 5 * time.Second

// RefusedFunc returns, when called, every database a local target must
// not share a cluster with: the monitored databases (fleet members added
// after start included) and the control (metadata) database.
type RefusedFunc func(ctx context.Context) ([]Querier, error)

// targetConn is the short-lived connection to the local target.
type targetConn interface {
	Querier
	Close(ctx context.Context) error
}

// LocalProvider is the development fallback when no clone provider is
// configured: a disposable database the operator names and marks, on a
// cluster of its own (the fault programs act on the whole cluster).
// Destroy leaves it in place (the fault programs clean up after
// themselves). It is a shared target: runs on it take turns (Leases).
type LocalProvider struct {
	dsn      string
	endpoint string
	cluster  string // host:port, the target key
	database string
	refused  RefusedFunc
	connect  func(ctx context.Context, dsn string) (targetConn, error)
	lock     func(ctx context.Context, dsn, name string) (func(), bool, error)
}

var (
	_ clone.Provider     = (*LocalProvider)(nil)
	_ clone.SharedTarget = (*LocalProvider)(nil)
)

// NewLocalProvider refuses at boot a DSN that names a monitored database
// (same host, port and database name); Create then compares cluster
// identities against refused, which must not be nil.
func NewLocalProvider(dsn string, monitored []string, refused RefusedFunc) (*LocalProvider,
	error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, ErrNotConfigured
	}
	if refused == nil {
		return nil, errors.New("game day local_dsn: no monitored databases to compare it with")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("game day local_dsn: not a PostgreSQL DSN")
	}
	target := endpointOfConfig(cfg)
	for _, m := range monitored {
		if e, err := endpointOf(m); err == nil && e == target {
			return nil, fmt.Errorf("%w: %s", ErrMonitoredDSN, target)
		}
	}
	if other := otherServer(cfg); other != "" {
		return nil, fmt.Errorf("%w: local_dsn names more than one server (%s:%d and %s); "+
			"Create verifies one, so name the one disposable server", ErrNotDisposable,
			strings.ToLower(cfg.Host), cfg.Port, other)
	}
	return &LocalProvider{dsn: dsn, endpoint: target, database: cfg.Database,
		cluster: fmt.Sprintf("%s:%d", strings.ToLower(cfg.Host), cfg.Port),
		refused: refused, connect: connectTarget, lock: clusterlock.TryLock}, nil
}

// otherServer is a fallback server of cfg other than its first ("" when
// every fallback is the first server again, as sslmode=prefer makes one).
// Create verifies one server, so the fault programs must never fail over
// to another.
func otherServer(cfg *pgx.ConnConfig) string {
	for _, fb := range cfg.Fallbacks {
		if !strings.EqualFold(fb.Host, cfg.Host) || fb.Port != cfg.Port {
			return fmt.Sprintf("%s:%d", strings.ToLower(fb.Host), fb.Port)
		}
	}
	return ""
}

// Create hands out the local database once it is proven disposable: on
// none of the refused clusters, a primary without standbys, and marked.
// Anything it cannot read refuses (fail closed).
func (p *LocalProvider) Create(ctx context.Context, _ clone.CloneSpec) (clone.Clone, error) {
	facts, err := p.readTarget(ctx)
	if err != nil {
		return clone.Clone{}, fmt.Errorf("%w: cannot verify %s: %w", ErrNotDisposable,
			p.endpoint, err)
	}
	if err := p.checkRefused(ctx, facts.id); err != nil {
		return clone.Clone{}, err
	}
	if err := p.checkDisposable(facts); err != nil {
		return clone.Clone{}, err
	}
	return clone.Clone{DSN: p.dsn, ID: "local", CreatedFrom: time.Now().UTC()}, nil
}

// TargetKey is the target's cluster (host:port): the fault programs act
// on the whole cluster, so every database on it is one target.
func (p *LocalProvider) TargetKey() string { return p.cluster }

// lockTarget takes the bench_target cluster lock, bounded by probeTimeout.
func (p *LocalProvider) lockTarget(ctx context.Context) (func(), bool, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return p.lock(ctx, p.dsn, BenchTargetLock)
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
	return endpointOfConfig(cfg), nil
}

func endpointOfConfig(cfg *pgx.ConnConfig) string {
	return fmt.Sprintf("%s:%d/%s", strings.ToLower(cfg.Host), cfg.Port, cfg.Database)
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
