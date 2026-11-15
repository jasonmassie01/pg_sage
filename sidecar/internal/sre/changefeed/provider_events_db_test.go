package changefeed

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Provider events against PostgreSQL (guard-02b): the real
// server_version_num, inet_server_addr(), pg_control_system() and
// pg_postmaster_start_time() seed the state rows; version and host
// changes are injected as a recorded prior state, since a test server
// cannot change its major version or host. Operator changes are dated
// months back, outside the 7-day window the cursor sources read, so no
// other test sees them. Event ids are unique per deployment (which the
// tests share), so each test injects its own prior address.

func providerPoller(t *testing.T, f pollFixture, managed bool, now time.Time) *Poller {
	t.Helper()
	p, err := NewPoller(PollerDeps{Feed: f.feed, Monitored: f.pool, Control: f.pool,
		Interval: time.Minute, ProviderEventsOnly: true, Managed: managed,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return p
}

func (f pollFixture) state(t *testing.T, source string) map[string]any {
	t.Helper()
	var raw []byte
	err := f.pool.QueryRow(f.ctx, `SELECT state::text FROM sage.sre_change_feed_state
		WHERE deployment_id = $1 AND database_id = $2 AND source = $3`,
		string(f.scope.DeploymentID), string(f.scope.DatabaseID), source).Scan(&raw)
	if err != nil {
		t.Fatalf("state of %s: %v", source, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("state of %s: %v", source, err)
	}
	return out
}

// around lists the scope's events of kind within an hour of at.
func (f pollFixture) around(t *testing.T, at time.Time, kind Kind) []Event {
	t.Helper()
	all, err := f.feed.store.List(f.ctx, f.scope, Filter{Since: at.Add(-time.Hour),
		Until: at.Add(time.Hour), Limit: MaxList})
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

// operatorChange records a config_audit row at `at` and removes it after
// the test.
func (f pollFixture) operatorChange(t *testing.T, at time.Time) {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.config_audit
		(key, old_value, new_value, changed_by_actor, changed_at)
		VALUES ('guard02b.test', 'a', 'b', 'admin', $1) RETURNING id::int8`,
		at).Scan(&id); err != nil {
		t.Fatalf("config_audit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`DELETE FROM sage.config_audit WHERE id = $1`, id)
	})
}

// The baseline poll reads the real catalog into the state rows and emits
// nothing.
func TestProviderEvent_DBBaselineFromRealCatalog(t *testing.T) {
	f := newPollFixture(t)
	p := providerPoller(t, f, false, time.Now())
	res, err := p.PollOnce(f.ctx)
	if err != nil || res.Emitted != 0 || len(res.Unavailable) != 0 {
		t.Fatalf("baseline: %+v err=%v", res, err)
	}
	var num, sysid string
	var addr *string
	if err := f.pool.QueryRow(f.ctx, `SELECT current_setting('server_version_num'),
		host(inet_server_addr()), (SELECT system_identifier::text FROM pg_control_system())`).
		Scan(&num, &addr, &sysid); err != nil {
		t.Fatal(err)
	}
	vs := f.state(t, "server_version")
	if got := strconv.FormatFloat(vs["num"].(float64), 'f', 0, 64); got != num {
		t.Fatalf("version state %v, server_version_num %s", vs, num)
	}
	if vs["start"] == "" || vs["start"] == nil {
		t.Fatalf("version state without the postmaster start: %v", vs)
	}
	hs := f.state(t, "server_host")
	wantAddr := ""
	if addr != nil {
		wantAddr = *addr
	}
	if hs["addr"] != wantAddr || hs["system_id"] != sysid {
		t.Fatalf("host state %v, want addr %q system_id %s", hs, wantAddr, sysid)
	}
	for _, src := range []string{"postmaster", "recovery_role", "timeline"} {
		_ = f.state(t, src)
	}
	if again, err := p.PollOnce(f.ctx); err != nil || again.Emitted != 0 {
		t.Fatalf("unchanged poll: %+v err=%v", again, err)
	}
}

// Injected prior version and host: one version_change (dated by the real
// postmaster start) and one host_change, each attributed; a re-poll emits
// nothing. (Restart ids are the real start time, which an earlier test of
// the shared deployment already recorded; the restart source's events go
// through the same attribution.)
func TestProviderEvent_DBInjectedChanges(t *testing.T) {
	f := newPollFixture(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := providerPoller(t, f, false, at)
	if _, err := p.PollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	cur := int(f.state(t, "server_version")["num"].(float64))
	f.setState(t, "server_version", `{"num":`+strconv.Itoa(cur-1)+
		`,"start":"2020-01-01T00:00:00Z"}`)
	f.setState(t, "server_host", `{"addr":"192.0.2.1","system_id":"1"}`)
	f.operatorChange(t, at.Add(-9*time.Minute-59*time.Second))
	res, err := p.PollOnce(f.ctx)
	if err != nil || res.Emitted != 2 {
		t.Fatalf("poll: %+v err=%v, want the version and the host change", res, err)
	}
	var start time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT pg_postmaster_start_time()`).
		Scan(&start); err != nil {
		t.Fatal(err)
	}
	vc := f.around(t, start, KindVersionChange)
	if len(vc) != 1 || !vc[0].OccurredAt.Equal(start.UTC()) || VersionChangeMajor(vc[0]) ||
		vc[0].Attribution != AttributionUnknown {
		t.Fatalf("version events = %+v, want one minor at %v, unknown", vc, start)
	}
	hc := f.around(t, at, KindHostChange)
	if len(hc) != 1 || hc[0].Attribution != AttributionOperator {
		t.Fatalf("host events = %+v, want one, operator (9m59s after a change)", hc)
	}
	if again, err := p.PollOnce(f.ctx); err != nil || again.Emitted != 0 {
		t.Fatalf("re-poll: %+v err=%v", again, err)
	}
}

// Boundary on PostgreSQL: 10m01s after an operator change, a managed
// database's host change is the provider's; a pending-restart config
// apply by pg_sage 5 minutes before is the operator's.
func TestProviderEvent_DBAttributionBoundary(t *testing.T) {
	f := newPollFixture(t)
	at := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	p := providerPoller(t, f, true, at)
	if _, err := p.PollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.operatorChange(t, at.Add(-10*time.Minute-time.Second))
	f.setState(t, "server_host", `{"addr":"192.0.2.11","system_id":"1"}`)
	if _, err := p.PollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if hc := f.around(t, at, KindHostChange); len(hc) != 1 ||
		hc[0].Attribution != AttributionProvider {
		t.Fatalf("host events = %+v, want one, provider", hc)
	}
	later := at.Add(24 * time.Hour)
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, executed_at)
		VALUES ('alter_system_guc', 'ALTER SYSTEM SET shared_buffers = ''1GB''',
		        'applied_pending_restart', $1) RETURNING id::int8`,
		later.Add(-5*time.Minute)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM sage.action_log WHERE id = $1`,
			id)
	})
	p2 := providerPoller(t, f, true, later)
	f.setState(t, "server_host", `{"addr":"192.0.2.12","system_id":"1"}`)
	if _, err := p2.PollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if hc := f.around(t, later, KindHostChange); len(hc) != 1 ||
		hc[0].Attribution != AttributionOperator {
		t.Fatalf("host events after a pending-restart apply = %+v, want operator", hc)
	}
}

// Concurrency: two pollers after a leader change read the same state row
// and record each change once.
func TestProviderEvent_DBTwoPollersEmitOnce(t *testing.T) {
	f := newPollFixture(t)
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	a, b := providerPoller(t, f, true, at), providerPoller(t, f, true, at)
	if _, err := a.PollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	cur := int(f.state(t, "server_version")["num"].(float64))
	f.setState(t, "server_version", `{"num":`+strconv.Itoa(cur-2)+
		`,"start":"2020-01-01T00:00:00Z"}`)
	f.setState(t, "server_host", `{"addr":"192.0.2.21","system_id":"1"}`)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, p := range []*Poller{a, b} {
		wg.Add(1)
		go func(i int, p *Poller) {
			defer wg.Done()
			_, errs[i] = p.PollOnce(f.ctx)
		}(i, p)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("pollers: %v, %v", errs[0], errs[1])
	}
	if hc := f.around(t, at, KindHostChange); len(hc) != 1 {
		t.Fatalf("host events = %+v, want exactly one", hc)
	}
	var start time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT pg_postmaster_start_time()`).
		Scan(&start); err != nil {
		t.Fatal(err)
	}
	if vc := f.around(t, start, KindVersionChange); len(vc) != 1 {
		t.Fatalf("version events = %+v, want exactly one", vc)
	}
}

// Error propagation on PostgreSQL: without EXECUTE on pg_control_system()
// the host source keeps the address (and the last known identifier) and
// is not reported unavailable. The revoke is per database (the fixture's).
func TestProviderEvent_DBControlSystemDenied(t *testing.T) {
	f := newPollFixture(t)
	f.exec(t, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'guard02b_reader') THEN
			CREATE ROLE guard02b_reader NOLOGIN;
		END IF; END $$`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP ROLE IF EXISTS guard02b_reader`)
	})
	f.exec(t, `REVOKE EXECUTE ON FUNCTION pg_control_system() FROM PUBLIC`)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(),
			`GRANT EXECUTE ON FUNCTION pg_control_system() TO PUBLIC`)
	})
	cfg := f.pool.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE guard02b_reader`)
		return err
	}
	reader, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	p, err := NewPoller(PollerDeps{Feed: f.feed, Monitored: reader, Interval: time.Minute,
		ProviderEventsOnly: true, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO sage.sre_change_feed_state (deployment_id, database_id, source,
		state) VALUES ($1, $2, 'server_host', '{"addr":"192.0.2.31","system_id":"42"}')`,
		string(f.scope.DeploymentID), string(f.scope.DatabaseID))
	res, err := p.PollOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reason, bad := res.Unavailable["server_host"]; bad {
		t.Fatalf("server_host unavailable (%s) without pg_control_system", reason)
	}
	if hs := f.state(t, "server_host"); hs["system_id"] != "42" {
		t.Fatalf("host state %v, want the last known identifier kept", hs)
	}
	var addr *string
	if err := reader.QueryRow(f.ctx, `SELECT host(inet_server_addr())`).Scan(&addr); err != nil {
		t.Fatal(err)
	}
	hc := f.around(t, time.Now(), KindHostChange)
	if addr != nil && len(hc) != 1 {
		t.Fatalf("address 192.0.2.31 -> %s: host events = %+v, want one", *addr, hc)
	}
	// pg_control_checkpoint() is not revoked: the timeline source still reads.
	if reason := res.Unavailable["timeline"]; reason != "" {
		t.Fatalf("timeline unavailable: %s", reason)
	}
}

// A provider API failover on a managed database lands once with source
// provider_api, attributed to the provider.
func TestProviderEvent_DBProviderAPIEvent(t *testing.T) {
	f := newPollFixture(t)
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	r := &fakeReader{events: []ProviderEvent{{ID: "rds:orders-db:" + string(f.scope.DatabaseID),
		Kind: KindFailover, Summary: "A Multi-AZ failover has completed.",
		At: at.Add(-time.Hour)}}}
	p, err := NewPoller(PollerDeps{Feed: f.feed, Monitored: f.pool, Control: f.pool,
		Interval: time.Minute, ProviderEventsOnly: true, Managed: true, ProviderAPI: r,
		Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := p.PollOnce(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	fo := f.around(t, at.Add(-time.Hour), KindFailover)
	if len(fo) != 1 || fo[0].Source != SourceProviderAPI ||
		fo[0].Attribution != AttributionProvider {
		t.Fatalf("provider failover events = %+v, want one provider_api, provider", fo)
	}
}
