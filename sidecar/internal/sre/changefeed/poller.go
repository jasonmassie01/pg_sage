package changefeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Poller turns pg_sage's own change sources into feed events (change
// feed v0). Cursor sources (actions, config audit, migration findings)
// emit one event per new row; snapshot sources (postmaster start,
// recovery role, timeline, pg_stat_statements reset, extensions) record
// a baseline on first sight and emit an event when it changes. Event ids
// come from the change itself (row ids, start times, versions), so a poll
// repeated after a crash records it once; only a recovery-role flip is
// keyed by the time it was seen.
type Poller struct {
	feed      *Feed
	monitored *pgxpool.Pool
	control   *pgxpool.Pool
	legacyID  *int
	interval  time.Duration
	retention time.Duration
	logf      func(level, msg string, args ...any)
	now       func() time.Time
}

// PollerDeps wires a poller. Control holds sage.config_audit (nil skips
// that source); Retention ages out the feed (zero keeps 90 days).
type PollerDeps struct {
	Feed             *Feed
	Monitored        *pgxpool.Pool
	Control          *pgxpool.Pool
	LegacyDatabaseID *int
	Interval         time.Duration
	Retention        time.Duration
	Logf             func(level, msg string, args ...any)
	Now              func() time.Time
}

// DefaultRetention is how long feed events are kept.
const DefaultRetention = 90 * 24 * time.Hour

// sourceTimeout bounds one source's reads.
const sourceTimeout = 5 * time.Second

// NewPoller validates its dependencies.
func NewPoller(d PollerDeps) (*Poller, error) {
	if d.Feed == nil || d.Monitored == nil || d.Interval <= 0 {
		return nil, fmt.Errorf("%w: poller needs a feed, the monitored pool and an interval",
			sre.ErrInvalidRequest)
	}
	p := &Poller{feed: d.Feed, monitored: d.Monitored, control: d.Control,
		legacyID: d.LegacyDatabaseID, interval: d.Interval, retention: d.Retention,
		logf: d.Logf, now: d.Now}
	if p.retention <= 0 {
		p.retention = DefaultRetention
	}
	if p.logf == nil {
		p.logf = func(string, string, ...any) {}
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p, nil
}

// PollResult is what one poll did: events newly recorded and the sources
// it could not read, with their reasons (no_privilege, unsupported,
// not_configured or error).
type PollResult struct {
	Emitted     int
	Unavailable map[string]string
}

// source is one change source: poll reads it given its previous state
// (nil on first sight) and returns new events and the state to keep.
type source struct {
	name    string
	control bool
	poll    func(ctx context.Context, q querier, prev []byte, now time.Time) ([]Event, any, error)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PollOnce polls every source once. Only an unbound scope or an
// unwritable feed is an error; a source that cannot be read is reported
// in Unavailable and the other sources still run.
func (p *Poller) PollOnce(ctx context.Context) (PollResult, error) {
	res := PollResult{Unavailable: map[string]string{}}
	scope, err := p.feed.bound(ctx)
	if err != nil {
		return res, err
	}
	for _, src := range p.sources() {
		n, reason, err := p.pollSource(ctx, scope, src)
		if err != nil {
			return res, err
		}
		res.Emitted += n
		if reason != "" {
			res.Unavailable[src.name] = reason
		}
	}
	return res, nil
}

func (p *Poller) pollSource(ctx context.Context, scope sre.Scope, src source) (int, string,
	error) {
	pool := p.monitored
	if src.control {
		if p.control == nil {
			return 0, "not_configured", nil
		}
		pool = p.control
	}
	prev, err := p.loadState(ctx, scope, src.name)
	if err != nil {
		return 0, "", err
	}
	sctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	events, state, err := src.poll(sctx, pool, prev, p.now())
	cancel()
	if err != nil {
		reason := classify(err)
		p.logf("DEBUG", "sre change feed: source %s unavailable (%s): %v", src.name, reason, err)
		return 0, reason, nil
	}
	emitted := 0
	for _, e := range events {
		_, created, err := p.feed.store.Record(ctx, scope, true, e)
		if err != nil && !errors.Is(err, ErrConflict) {
			return emitted, "", err
		}
		if created {
			emitted++
		}
	}
	return emitted, "", p.saveState(ctx, scope, src.name, state)
}

// classify maps a source error to its unavailability reason.
func classify(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42501":
			return "no_privilege"
		case "42P01", "42883", "55000", "0A000":
			return "unsupported"
		}
	}
	return "error"
}

func (p *Poller) loadState(ctx context.Context, scope sre.Scope, name string) ([]byte, error) {
	var raw []byte
	err := p.feed.store.pool.QueryRow(ctx, `SELECT state::text FROM sage.sre_change_feed_state
		WHERE deployment_id = $1 AND database_id = $2 AND source = $3`,
		string(scope.DeploymentID), string(scope.DatabaseID), name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read change feed state of %s: %w", name, err)
	}
	return raw, nil
}

func (p *Poller) saveState(ctx context.Context, scope sre.Scope, name string, state any) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode change feed state of %s: %w", name, err)
	}
	_, err = p.feed.store.pool.Exec(ctx, `INSERT INTO sage.sre_change_feed_state
		(deployment_id, database_id, source, state) VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (deployment_id, database_id, source)
		DO UPDATE SET state = EXCLUDED.state, updated_at = clock_timestamp()`,
		string(scope.DeploymentID), string(scope.DatabaseID), name, string(raw))
	if err != nil {
		return fmt.Errorf("save change feed state of %s: %w", name, err)
	}
	return nil
}

// purgeInterval spaces retention passes.
const purgeInterval = time.Hour

// Run polls every interval and ages out the feed hourly until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	var lastPurge time.Time
	for {
		p.tick(ctx, &lastPurge)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Poller) tick(ctx context.Context, lastPurge *time.Time) {
	if ctx.Err() != nil {
		return
	}
	if _, err := p.PollOnce(ctx); err != nil && ctx.Err() == nil {
		p.logf("WARN", "sre change feed of %s: poll failed: %v", p.feed.name, err)
		return
	}
	if time.Since(*lastPurge) < purgeInterval {
		return
	}
	scope, err := p.feed.bound(ctx)
	if err != nil {
		return
	}
	n, err := p.feed.store.Purge(ctx, scope.DeploymentID, p.now().Add(-p.retention))
	switch {
	case err != nil:
		p.logf("WARN", "sre change feed: retention failed: %v", err)
	case n > 0:
		p.logf("INFO", "sre change feed: retention deleted %d events", n)
	}
	*lastPurge = time.Now()
}
