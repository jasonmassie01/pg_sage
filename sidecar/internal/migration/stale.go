package migration

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultStaleFindingAge is how long a migration_safety finding may go
// without re-detection before it is resolved (G7-B13).
const DefaultStaleFindingAge = 24 * time.Hour

// staleCheckInterval rate-limits the resolver inside detector loops.
const staleCheckInterval = 10 * time.Minute

// ResolveStaleFindings resolves open migration_safety findings whose
// DDL has not been re-detected for maxAge, so they leave the open list
// and become eligible for retention. Returns the number resolved.
func ResolveStaleFindings(
	ctx context.Context, pool *pgxpool.Pool, maxAge time.Duration,
) (int64, error) {
	if pool == nil {
		return 0, fmt.Errorf("resolve stale migration findings: pool is nil")
	}
	if maxAge <= 0 {
		maxAge = DefaultStaleFindingAge
	}
	tag, err := pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.findings
		    SET status = 'resolved', resolved_at = now()
		  WHERE category = $1
		    AND status = 'open'
		    AND last_seen < now() - make_interval(secs => $2)`,
		SafetyFindingCategory, maxAge.Seconds())
	if err != nil {
		return 0, fmt.Errorf("resolve stale migration findings: %w", err)
	}
	return tag.RowsAffected(), nil
}

// StaleResolver runs ResolveStaleFindings at most once per interval. One
// database has one resolver, shared by the activity detector and the log
// detector (migration-12), so it is safe for concurrent use: the interval
// is claimed under the lock and the UPDATE runs outside it.
type StaleResolver struct {
	mu      sync.Mutex
	resolve func(context.Context) (int64, error)
	logFn   func(string, string, ...any)
	lastRun time.Time
	now     func() time.Time
}

// NewStaleResolver returns the resolver for one database's findings, or
// nil without a pool (a nil resolver does nothing).
func NewStaleResolver(
	pool *pgxpool.Pool, logFn func(string, string, ...any),
) *StaleResolver {
	if pool == nil {
		return nil
	}
	return &StaleResolver{
		resolve: func(ctx context.Context) (int64, error) {
			return ResolveStaleFindings(ctx, pool, DefaultStaleFindingAge)
		},
		logFn: logFn, now: time.Now,
	}
}

// claim reports whether this call owns the current interval's run.
func (r *StaleResolver) claim() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if !r.lastRun.IsZero() && now.Sub(r.lastRun) < staleCheckInterval {
		return false
	}
	r.lastRun = now
	return true
}

func (r *StaleResolver) maybeRun(ctx context.Context) {
	if r == nil || r.resolve == nil || !r.claim() {
		return
	}
	n, err := r.resolve(ctx)
	if err != nil {
		r.logFn("warn", "migration: %v", err)
		return
	}
	if n > 0 {
		r.logFn("info", "migration: resolved %d stale migration findings", n)
	}
}
