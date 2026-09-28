package migration

import (
	"context"
	"fmt"
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

// staleResolver runs ResolveStaleFindings at most once per interval.
type staleResolver struct {
	pool    *pgxpool.Pool
	logFn   func(string, string, ...any)
	lastRun time.Time
	now     func() time.Time
}

func newStaleResolver(
	pool *pgxpool.Pool, logFn func(string, string, ...any),
) *staleResolver {
	return &staleResolver{pool: pool, logFn: logFn, now: time.Now}
}

func (r *staleResolver) maybeRun(ctx context.Context) {
	if r == nil || r.pool == nil || r.now().Sub(r.lastRun) < staleCheckInterval {
		return
	}
	r.lastRun = r.now()
	n, err := ResolveStaleFindings(ctx, r.pool, DefaultStaleFindingAge)
	if err != nil {
		r.logFn("warn", "migration: %v", err)
		return
	}
	if n > 0 {
		r.logFn("info", "migration: resolved %d stale migration findings", n)
	}
}
