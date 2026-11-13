// Package agenttools is what pg_sage hands a coding agent about one
// monitored database (roadmap phase 3, MCP v2): the workload with its
// application attribution, safe EXPLAIN, HypoPG what-ifs, migration lint,
// ownership marks, and the source-fix loop (a packet describing the change
// for the application's migrations, then the agent's report of the pull
// request and deploy, then pg_sage's verdict on what the deploy did).
//
// Nothing here changes the monitored database's schema or data: marks
// are proposals an operator decides, and source fixes are run by the
// application's own migrations, never by pg_sage.
package agenttools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Errors. Each is distinguishable with errors.Is; details are wrapped.
var (
	ErrInvalid     = errors.New("agenttools: invalid request")
	ErrNotFound    = errors.New("agenttools: not found")
	ErrUnavailable = errors.New("agenttools: unavailable")
	ErrNoChange    = errors.New("agenttools: finding has no source change")
	ErrTransition  = errors.New("agenttools: report not allowed in this stage")
)

// DefaultVerifyWindow is the before/after window of a source-fix verdict.
const DefaultVerifyWindow = 2 * time.Hour

// Options configure Tools; zero values mean the defaults.
type Options struct {
	// Explain bounds EXPLAIN (timeout, cache TTL); nil means defaults.
	Explain *config.ExplainConfig
	// PlanMaxAgeMinutes is tuner.plan_max_age_minutes: a captured plan this
	// old is not used for a query_id (0 or less means the default).
	PlanMaxAgeMinutes int
	// VerifyWindow is the window on each side of a deploy; 0 means 2h.
	VerifyWindow time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Excluded reports statements that are not application workload;
	// nil means the workload rule (workload.Excluded).
	Excluded func(query string) bool
	// Log receives diagnostics; nil discards them.
	Log func(level, format string, args ...any)
	// DDLRowThreshold is migration.ddl_row_threshold for lint_migration
	// (0 means the default, 10000).
	DDLRowThreshold int
}

// Tools are the coding-agent tools of one monitored database.
type Tools struct {
	pool *pgxpool.Pool
	opts Options
	// sqlWorkload is true when opts.Excluded is the workload rule, which
	// readers can then also apply in SQL (workload.AdviceSQL) before a
	// bounded scan, so pg_sage's own statements cannot crowd it out.
	sqlWorkload bool
}

// New returns the tools of the database behind pool.
func New(pool *pgxpool.Pool, opts Options) *Tools {
	if opts.VerifyWindow <= 0 {
		opts.VerifyWindow = DefaultVerifyWindow
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	sqlWorkload := opts.Excluded == nil
	if sqlWorkload {
		opts.Excluded = workload.Excluded
	}
	if opts.Log == nil {
		opts.Log = func(string, string, ...any) {}
	}
	return &Tools{pool: pool, opts: opts, sqlWorkload: sqlWorkload}
}

func (t *Tools) now() time.Time { return t.opts.Now() }

// ready fails when the tools have no database pool.
func (t *Tools) ready() error {
	if t == nil || t.pool == nil {
		return fmt.Errorf("%w: no database pool", ErrUnavailable)
	}
	return nil
}

// invalid wraps a request problem.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// currentDatabase is the name of the database behind the pool.
func (t *Tools) currentDatabase(ctx context.Context) (string, error) {
	var name string
	err := t.pool.QueryRow(ctx, "/* pg_sage */ SELECT current_database()").Scan(&name)
	if err != nil {
		return "", fmt.Errorf("read current database: %w", err)
	}
	return name, nil
}
