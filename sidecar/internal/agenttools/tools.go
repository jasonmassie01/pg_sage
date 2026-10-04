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
	"github.com/pg-sage/sidecar/internal/selfmonitor"
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
	// VerifyWindow is the window on each side of a deploy; 0 means 2h.
	VerifyWindow time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Excluded reports statements that are not application workload;
	// nil means pg_sage's own statements.
	Excluded func(query string) bool
	// Log receives diagnostics; nil discards them.
	Log func(level, format string, args ...any)
}

// Tools are the coding-agent tools of one monitored database.
type Tools struct {
	pool *pgxpool.Pool
	opts Options
}

// New returns the tools of the database behind pool.
func New(pool *pgxpool.Pool, opts Options) *Tools {
	if opts.VerifyWindow <= 0 {
		opts.VerifyWindow = DefaultVerifyWindow
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Excluded == nil {
		// TODO(#110): use workload.Excluded once merged
		opts.Excluded = selfmonitor.IsQueryText
	}
	if opts.Log == nil {
		opts.Log = func(string, string, ...any) {}
	}
	return &Tools{pool: pool, opts: opts}
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
