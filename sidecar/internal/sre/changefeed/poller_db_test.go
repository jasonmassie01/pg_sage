package changefeed

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Change feed v0 (AI-SRE-SPEC §4 R1): pg_sage's own actions, config
// changes (sage.config_audit), DDL seen by the migration detector,
// pg_stat_statements resets, restarts, failovers (recovery role and
// timeline id) and extension version changes become change events. The
// first poll records a baseline for the snapshot sources (no events);
// later polls emit one event per change, idempotently. A source that
// cannot be read is reported unavailable, never silently healthy.

type pollFixture struct {
	pool   *pgxpool.Pool
	scope  sre.Scope
	feed   *Feed
	poller *Poller
	ctx    context.Context
}

func newPollFixture(t *testing.T) pollFixture {
	t.Helper()
	st, scope, ctx := liveStore(t)
	feed := NewFeed(st, "orders", func(context.Context) (sre.Scope, error) {
		return scope, nil
	})
	p, err := NewPoller(PollerDeps{Feed: feed, Monitored: st.pool, Control: st.pool,
		Interval: time.Minute, Retention: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return pollFixture{pool: st.pool, scope: scope, feed: feed, poller: p, ctx: ctx}
}

func (f pollFixture) poll(t *testing.T) PollResult {
	t.Helper()
	res, err := f.poller.PollOnce(f.ctx)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	return res
}

func (f pollFixture) events(t *testing.T, kind Kind) []Event {
	t.Helper()
	all, err := f.feed.Recent(f.ctx, 8*24*time.Hour, MaxList)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range all {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (f pollFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (f pollFixture) setState(t *testing.T, source, state string) {
	t.Helper()
	f.exec(t, `UPDATE sage.sre_change_feed_state SET state = $4::jsonb
		WHERE deployment_id = $1 AND database_id = $2 AND source = $3`,
		string(f.scope.DeploymentID), string(f.scope.DatabaseID), source, state)
}

func TestNewPoller_RequiresItsDependencies(t *testing.T) {
	f := newPollFixture(t)
	for name, d := range map[string]PollerDeps{
		"no feed":      {Monitored: f.pool, Control: f.pool, Interval: time.Minute},
		"no monitored": {Feed: f.feed, Control: f.pool, Interval: time.Minute},
		"no interval":  {Feed: f.feed, Monitored: f.pool, Control: f.pool},
	} {
		if _, err := NewPoller(d); err == nil {
			t.Errorf("%s: NewPoller succeeded", name)
		}
	}
	if _, err := NewPoller(PollerDeps{Feed: f.feed, Monitored: f.pool,
		Interval: time.Minute}); err != nil {
		t.Errorf("without a control pool (no config audit): %v", err)
	}
}

// The first poll records the snapshot baseline without inventing changes;
// a second poll with nothing changed emits nothing.
func TestPoller_BaselineThenNoChange(t *testing.T) {
	f := newPollFixture(t)
	f.poll(t)
	for _, k := range []Kind{KindRestart, KindFailover, KindExtension, KindStatsReset} {
		if evs := f.events(t, k); len(evs) != 0 {
			t.Fatalf("baseline poll emitted %d %s events", len(evs), k)
		}
	}
	res := f.poll(t)
	if res.Emitted != 0 {
		t.Fatalf("unchanged poll emitted %d events", res.Emitted)
	}
	var sources int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.sre_change_feed_state
		WHERE deployment_id = $1 AND database_id = $2`, string(f.scope.DeploymentID),
		string(f.scope.DatabaseID)).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if sources < 5 {
		t.Fatalf("%d sources kept state, want the snapshot and cursor sources", sources)
	}
}

func TestPoller_SageActionsConfigAndDDL(t *testing.T) {
	f := newPollFixture(t)
	f.poll(t)
	f.exec(t, `INSERT INTO sage.action_log (action_type, sql_executed, outcome)
		VALUES ('create_index', 'CREATE INDEX CONCURRENTLY secret_idx ON t (x)', 'success')`)
	f.exec(t, `INSERT INTO sage.config_audit (key, old_value, new_value, changed_by_actor)
		VALUES ('llm.api_key', 'sk-old-secret', 'sk-new-secret', 'admin')`)
	f.exec(t, `INSERT INTO sage.findings (category, severity, object_type,
		    object_identifier, title, detail, recommendation, status)
		VALUES ('migration_safety', 'warning', 'migration', 'mig-1',
		    'ALTER TABLE rewrites public.orders', '{}'::jsonb, 'use a lock_timeout', 'open')`)
	res := f.poll(t)
	if res.Emitted < 3 {
		t.Fatalf("emitted %d events, want the action, the config change and the DDL",
			res.Emitted)
	}
	acts := f.events(t, KindSageAction)
	if len(acts) != 1 || !strings.Contains(acts[0].Summary, "create_index") ||
		acts[0].Signature != SignatureInternal || acts[0].Source != SourceSage {
		t.Fatalf("sage action events = %+v", acts)
	}
	cfg := f.events(t, KindConfig)
	if len(cfg) != 1 || !strings.Contains(cfg[0].Summary, "llm.api_key") {
		t.Fatalf("config events = %+v", cfg)
	}
	for _, e := range append(acts, cfg...) {
		if strings.Contains(e.Summary, "secret") || strings.Contains(e.Summary, "sk-") {
			t.Fatalf("event leaks SQL or a value: %q", e.Summary)
		}
	}
	ddl := f.events(t, KindDDL)
	if len(ddl) != 1 || !strings.Contains(ddl[0].Summary, "ALTER TABLE rewrites") {
		t.Fatalf("ddl events = %+v", ddl)
	}
	if again := f.poll(t); again.Emitted != 0 {
		t.Fatalf("re-poll emitted %d duplicate events", again.Emitted)
	}
}

// A changed postmaster start time is a restart; a changed recovery role
// or timeline is a failover; a changed extension version is an extension
// change. The baseline is rewritten to the old value to simulate them.
func TestPoller_SnapshotChanges(t *testing.T) {
	f := newPollFixture(t)
	f.poll(t)
	f.setState(t, "postmaster", `{"start":"2020-01-01T00:00:00Z"}`)
	f.setState(t, "recovery_role", `{"in_recovery":true}`)
	f.setState(t, "timeline", `{"timeline":0}`)
	f.setState(t, "extensions", `{"versions":{"plpgsql":"0.9","pg_gone":"1.0"}}`)
	res := f.poll(t)
	if res.Emitted < 4 {
		t.Fatalf("emitted %d, want restart, failover (role and timeline) and extensions",
			res.Emitted)
	}
	restarts := f.events(t, KindRestart)
	if len(restarts) != 1 || !strings.Contains(restarts[0].Summary, "restart") {
		t.Fatalf("restart events = %+v", restarts)
	}
	fo := f.events(t, KindFailover)
	if len(fo) != 2 {
		t.Fatalf("failover events = %+v, want role change and timeline change", fo)
	}
	ext := f.events(t, KindExtension)
	var upgraded, removed bool
	for _, e := range ext {
		upgraded = upgraded || strings.Contains(e.Summary, "plpgsql 0.9 -> ")
		removed = removed || strings.Contains(e.Summary, "pg_gone 1.0 removed")
	}
	if !upgraded || !removed {
		t.Fatalf("extension events = %+v", ext)
	}
	if again := f.poll(t); again.Emitted != 0 {
		t.Fatalf("re-poll emitted %d", again.Emitted)
	}
}

// pg_stat_statements resets are change events when the extension is
// there; without it the source is reported unavailable.
func TestPoller_StatsReset(t *testing.T) {
	f := newPollFixture(t)
	if _, err := f.pool.Exec(f.ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
		res := f.poll(t)
		if res.Unavailable["pg_stat_statements"] == "" {
			t.Fatalf("no extension: unavailable = %v", res.Unavailable)
		}
		t.Logf("pg_stat_statements cannot be created here (%v); checked the unavailable path", err)
		return
	}
	res := f.poll(t)
	if reason := res.Unavailable["pg_stat_statements"]; reason != "" {
		// Not preloaded: the view errors and the source is unavailable.
		t.Logf("pg_stat_statements unavailable: %s", reason)
		return
	}
	f.setState(t, "pg_stat_statements", `{"stats_reset":"2020-01-01T00:00:00Z"}`)
	f.poll(t)
	if evs := f.events(t, KindStatsReset); len(evs) != 1 {
		t.Fatalf("stats reset events = %+v", evs)
	}
}

// A role that cannot read pg_sage's own tables leaves those sources
// unavailable with a reason; the readable sources still run.
func TestPoller_UnreadableSourceIsUnavailable(t *testing.T) {
	f := newPollFixture(t)
	role := "cf_limited_" + strings.ReplaceAll(string(sre.NewUUID())[:8], "-", "")
	f.exec(t, `CREATE ROLE `+role+` LOGIN PASSWORD 'cf-test-pw'`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = f.pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	cfg := f.pool.Config().Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, "cf-test-pw"
	limited, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(limited.Close)
	p, err := NewPoller(PollerDeps{Feed: f.feed, Monitored: limited, Control: f.pool,
		Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.PollOnce(f.ctx)
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	for _, src := range []string{"sage_actions", "migration_findings"} {
		if res.Unavailable[src] != "no_privilege" {
			t.Errorf("%s: unavailable = %q, want no_privilege (all: %v)", src,
				res.Unavailable[src], res.Unavailable)
		}
	}
	if res.Unavailable["postmaster"] != "" || res.Unavailable["extensions"] != "" {
		t.Fatalf("readable sources reported unavailable: %v", res.Unavailable)
	}
}

// Without a bound scope the poll fails with a distinguishable error and
// records nothing.
func TestPoller_UnboundScope(t *testing.T) {
	f := newPollFixture(t)
	feed := NewFeed(f.feed.store, "orders", func(context.Context) (sre.Scope, error) {
		return sre.Scope{}, sre.ErrMetadataUnavailable
	})
	p, err := NewPoller(PollerDeps{Feed: feed, Monitored: f.pool, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.PollOnce(f.ctx); !errors.Is(err, sre.ErrMetadataUnavailable) {
		t.Fatalf("PollOnce: err = %v, want ErrMetadataUnavailable", err)
	}
}

// Run polls until its context ends.
func TestPoller_RunStopsWithContext(t *testing.T) {
	f := newPollFixture(t)
	p, err := NewPoller(PollerDeps{Feed: f.feed, Monitored: f.pool, Control: f.pool,
		Interval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.sre_change_feed_state
		WHERE database_id = $1`, string(f.scope.DatabaseID)).Scan(&n); err != nil || n == 0 {
		t.Fatalf("Run never polled: %d states, err %v", n, err)
	}
}
