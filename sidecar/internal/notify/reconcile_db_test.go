package notify

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// alerting-06b on real PostgreSQL: the reconciler writes config-managed
// rows on the control database under one advisory-locked transaction.
// Standalone and meta mode share this path: both hand the control pool
// to NewPoolStore, so one test covers both.

var reconcileTestKey = []byte("0123456789abcdef0123456789abcdef")

func resetConfigRows(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, q := range []string{
			`DELETE FROM sage.notification_channels WHERE managed_by = 'config'`,
			`DELETE FROM sage.notification_channels WHERE name LIKE 'config:%'`,
			`DELETE FROM sage.notification_channels WHERE name LIKE 'a06b-%'`,
		} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestReconcileDB_CreatesSealedRowsThenIsANoop(t *testing.T) {
	pool := connectTestDB(t)
	resetConfigRows(t, pool)
	store := NewPoolStore(pool, reconcileTestKey)
	ctx := context.Background()
	apiID := insertTestChannel(t, pool, "a06b-team", "slack",
		map[string]string{"webhook_url": testSlackURL}, true)
	res, err := Reconcile(ctx, store, baseAlerting(), TargetPolicy{}, nil)
	if err != nil || res != (ReconcileResult{ChannelsCreated: 3, RulesCreated: 6}) {
		t.Fatalf("first reconcile = %+v, %v", res, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sage.notification_channels
		WHERE managed_by = 'config' AND config_hash <> ''`); n != 3 {
		t.Fatalf("config channels with a hash = %d, want 3", n)
	}
	var raw string
	if err := pool.QueryRow(ctx, `SELECT config->>'routing_key'
		FROM sage.notification_channels WHERE name = 'config:pagerduty:pagerduty'`,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == testPDKey || !strings.HasPrefix(raw, "sage-enc:") {
		t.Fatalf("stored routing_key = %q, want it sealed", raw)
	}
	ch, err := store.ChannelByName(ctx, "config:pagerduty:pagerduty")
	if err != nil || ch.Config["routing_key"] != testPDKey || ch.ManagedBy != "config" {
		t.Fatalf("ChannelByName = %+v, %v; want the opened key", ch, err)
	}
	again, err := Reconcile(ctx, store, baseAlerting(), TargetPolicy{}, nil)
	if err != nil || again != (ReconcileResult{}) {
		t.Fatalf("second reconcile = %+v, %v; want all zero", again, err)
	}
	if _, err := Reconcile(ctx, store, configOff(), TargetPolicy{}, nil); err != nil {
		t.Fatalf("reconcile without a block: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sage.notification_channels
		WHERE id = $1 AND managed_by = 'api'`, apiID); n != 1 {
		t.Fatal("the API channel was touched by the reconciler")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sage.notification_rules
		WHERE managed_by = 'config'`); n != 0 {
		t.Fatalf("config rules left = %d, want 0", n)
	}
}

func TestReconcileDB_FailureRollsBackEveryWrite(t *testing.T) {
	pool := connectTestDB(t)
	resetConfigRows(t, pool)
	// An API channel already holds the webhook's reserved name, so its
	// insert fails after pagerduty and slack were written in the same tx.
	insertTestChannel(t, pool, "config:webhook:ops", "slack",
		map[string]string{"webhook_url": testSlackURL}, true)
	_, err := Reconcile(context.Background(), NewPoolStore(pool, nil), baseAlerting(),
		TargetPolicy{}, nil)
	if err == nil || !strings.Contains(err.Error(), `"config:webhook:ops"`) {
		t.Fatalf("err = %v, want the failing channel named", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sage.notification_channels
		WHERE managed_by = 'config'`); n != 0 {
		t.Fatalf("config channels after a failed reconcile = %d, want 0", n)
	}
}

func TestReconcileDB_TwoReconcilersEndWithOneRowPerTarget(t *testing.T) {
	pool := connectTestDB(t)
	resetConfigRows(t, pool)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := NewPoolStore(pool, reconcileTestKey)
			if _, err := Reconcile(context.Background(), store, baseAlerting(),
				TargetPolicy{}, nil); err != nil {
				t.Errorf("Reconcile: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := countRows(t, pool, `SELECT count(*) FROM sage.notification_channels
		WHERE managed_by = 'config'`); n != 3 {
		t.Fatalf("config channels = %d, want one per target", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sage.notification_rules
		WHERE managed_by = 'config'`); n != 6 {
		t.Fatalf("config rules = %d, want 6", n)
	}
}

func configOff() config.AlertingConfig { return config.AlertingConfig{} }

// seedLegacyPage seeds a finding and the legacy PagerDuty trigger that paged
// it before the cutover, and returns the finding id and alert_log key.
func seedLegacyPage(t *testing.T, pool *pgxpool.Pool) (int64, string) {
	t.Helper()
	ctx := context.Background()
	object := fmt.Sprintf("public.t_%d", time.Now().UnixNano())
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_identifier, title, detail, status)
		VALUES ('a06b_bloat', 'critical', $1, 'Bloat', '{}', 'open') RETURNING id`,
		object).Scan(&id); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	key := "a06b_bloat:" + object
	if _, err := pool.Exec(ctx, `INSERT INTO sage.alert_log
		(finding_id, severity, channel, dedup_key, status)
		VALUES ($1, 'critical', 'pagerduty', $2, 'sent')`, id, key); err != nil {
		t.Fatalf("insert alert_log: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.alert_log WHERE finding_id = $1`, id)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.findings WHERE id = $1`, id)
	})
	return id, key
}

func cutoverRig(t *testing.T, statuses ...int) (*pgxpool.Pool, *Dispatcher, Channel, *statusSeq) {
	t.Helper()
	pool := connectTestDB(t)
	resetConfigRows(t, pool)
	store := NewPoolStore(pool, reconcileTestKey)
	ctx := context.Background()
	if _, err := Reconcile(ctx, store, baseAlerting(), TargetPolicy{}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pd := &statusSeq{statuses: statuses}
	srv := pd.server(t)
	d := NewDefaultDispatcher(store, TargetPolicy{AllowPrivate: true}, nil)
	d.RegisterSender(&PagerDutySender{client: srv.Client(), apiURL: srv.URL})
	ch, err := store.ChannelByName(ctx, ConfigChannelName("pagerduty", "pagerduty"))
	if err != nil {
		t.Fatalf("ChannelByName: %v", err)
	}
	return pool, d, *ch, pd
}

func TestLegacyCutoverDB_ResolvesUnderTheLegacyKey(t *testing.T) {
	pool, d, ch, pd := cutoverRig(t, 202)
	id, key := seedLegacyPage(t, pool)
	ctx := context.Background()
	res, err := ResolveLegacyPages(ctx, pool, "db1", d, ch)
	if err != nil || res != (LegacyCutoverResult{StillOpen: 1}) {
		t.Fatalf("open finding: %+v, %v; want still open, nothing sent", res, err)
	}
	if hits, _ := pd.counts(); hits != 0 {
		t.Fatalf("pagerduty hits = %d before the finding resolved", hits)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved',
		resolved_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	res, err = ResolveLegacyPages(ctx, pool, "db1", d, ch)
	if err != nil || res != (LegacyCutoverResult{Resolved: 1}) {
		t.Fatalf("resolved finding: %+v, %v; want one resolve", res, err)
	}
	pd.mu.Lock()
	body := pd.bodies[0]
	pd.mu.Unlock()
	if body["event_action"] != "resolve" || body["dedup_key"] != "db1:"+key ||
		body["routing_key"] != testPDKey {
		t.Fatalf("pagerduty body = %v, want a resolve on the legacy key db1:%s", body, key)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sage.alert_log
		WHERE finding_id = $1 AND channel = 'pagerduty' AND status = 'resolved'`, id); n != 1 {
		t.Fatalf("resolved alert_log rows = %d, want 1", n)
	}
	res, err = ResolveLegacyPages(ctx, pool, "db1", d, ch)
	if err != nil || res != (LegacyCutoverResult{}) {
		t.Fatalf("after the resolve: %+v, %v; want nothing left", res, err)
	}
	if hits, _ := pd.counts(); hits != 1 {
		t.Fatalf("pagerduty hits = %d, want the one resolve", hits)
	}
}

func TestLegacyCutoverDB_FailedResolveIsRetried(t *testing.T) {
	pool, d, ch, pd := cutoverRig(t, 500, 202)
	id, _ := seedLegacyPage(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved',
		resolved_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	res, err := ResolveLegacyPages(ctx, pool, "", d, ch)
	if err != nil || res != (LegacyCutoverResult{Failed: 1, StillOpen: 1}) {
		t.Fatalf("failed send: %+v, %v; want failed and still owed", res, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM sage.alert_log
		WHERE finding_id = $1 AND status = 'resolve_error'`, id); n != 1 {
		t.Fatalf("resolve_error rows = %d, want 1", n)
	}
	res, err = ResolveLegacyPages(ctx, pool, "", d, ch)
	if err != nil || res != (LegacyCutoverResult{Resolved: 1}) {
		t.Fatalf("retry: %+v, %v; want resolved", res, err)
	}
	pd.mu.Lock()
	key := pd.bodies[0]["dedup_key"]
	pd.mu.Unlock()
	if s, _ := key.(string); !strings.HasPrefix(s, "a06b_bloat:") {
		t.Fatalf("unnamed database key = %v, want <category>:<object>", key)
	}
}

func TestLegacyCutoverDB_NilPoolIsAnError(t *testing.T) {
	if _, err := ResolveLegacyPages(context.Background(), nil, "db1", nil,
		Channel{}); err == nil {
		t.Fatal("nil pool: want an error")
	}
}

func TestLegacyPagerDutyKey(t *testing.T) {
	if got := LegacyPagerDutyKey("db1", "bloat:t"); got != "db1:bloat:t" {
		t.Fatalf("named = %q", got)
	}
	if got := LegacyPagerDutyKey("", "bloat:t"); got != "bloat:t" {
		t.Fatalf("unnamed = %q", got)
	}
	long := LegacyPagerDutyKey("db", strings.Repeat("x", 400))
	if len(long) > 255 {
		t.Fatalf("key length %d exceeds the PagerDuty bound", len(long))
	}
}
