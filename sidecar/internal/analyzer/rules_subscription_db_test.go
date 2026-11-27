package analyzer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
)

// market-13 on a real server (wal_level=logical): a publisher and a
// subscriber database in one cluster, the subscription made with
// create_slot = false on a slot created by hand. A row that conflicts on the
// subscriber makes the apply worker fail on a unique violation; on PG15+
// apply_error_count rises, disable_on_error switches the subscription off,
// and subscription_errors fires. PG14 has no pg_stat_subscription_stats:
// the subscription is read without counters and without an error.

type subscriberFixture struct {
	pub, sub *pgxpool.Pool
	version  int
	subName  string
}

func dbPool(t *testing.T, base *pgxpool.Pool, db string) *pgxpool.Pool {
	t.Helper()
	cfg := base.Config().Copy()
	cfg.ConnConfig.Database = db
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect to %s: %v", db, err)
	}
	t.Cleanup(p.Close)
	return p
}

func mustExec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func newSubscriberFixture(t *testing.T) *subscriberFixture {
	t.Helper()
	admin := phase2Pool(t)
	ctx := context.Background()
	var version, port int
	var walLevel string
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,
		current_setting('wal_level'), current_setting('port')::int`).Scan(
		&version, &walLevel, &port); err != nil {
		t.Fatalf("read server settings: %v", err)
	}
	if walLevel != "logical" {
		t.Skipf("wal_level=%s: subscriptions need wal_level=logical", walLevel)
	}
	n := time.Now().UnixNano()
	f := &subscriberFixture{version: version, subName: fmt.Sprintf("m13_sub_%d", n)}
	pubDB, subDB, slot := fmt.Sprintf("m13_pub_%d", n), fmt.Sprintf("m13_sdb_%d", n),
		fmt.Sprintf("m13_slot_%d", n)
	mustExec(t, admin, "CREATE DATABASE "+pubDB)
	mustExec(t, admin, "CREATE DATABASE "+subDB)
	f.pub, f.sub = dbPool(t, admin, pubDB), dbPool(t, admin, subDB)
	t.Cleanup(func() { f.teardown(t, admin, pubDB, subDB, slot) })
	for _, p := range []*pgxpool.Pool{f.pub, f.sub} {
		mustExec(t, p, "CREATE TABLE m13_t (id int PRIMARY KEY)")
	}
	mustExec(t, f.pub, "CREATE PUBLICATION m13_pub FOR TABLE m13_t")
	mustExec(t, f.pub, "SELECT pg_create_logical_replication_slot($1, 'pgoutput')", slot)
	mustExec(t, f.sub, "INSERT INTO m13_t VALUES (1)")
	cc := admin.Config().ConnConfig
	conn := fmt.Sprintf("host=127.0.0.1 port=%d dbname=%s user=%s password=%s",
		port, pubDB, cc.User, cc.Password)
	opts := "create_slot = false, slot_name = '" + slot + "', copy_data = false"
	if version >= 150000 {
		opts += ", disable_on_error = true"
	}
	mustExec(t, f.sub, fmt.Sprintf("CREATE SUBSCRIPTION %s CONNECTION '%s' "+
		"PUBLICATION m13_pub WITH (%s)", f.subName, conn, opts))
	return f
}

func (f *subscriberFixture) teardown(t *testing.T, admin *pgxpool.Pool, pubDB, subDB,
	slot string) {
	ctx := context.Background()
	if f.sub != nil {
		for _, sql := range []string{"ALTER SUBSCRIPTION %s DISABLE",
			"ALTER SUBSCRIPTION %s SET (slot_name = NONE)", "DROP SUBSCRIPTION %s"} {
			if _, err := f.sub.Exec(ctx, fmt.Sprintf(sql, f.subName)); err != nil {
				t.Logf("teardown %s: %v", sql, err)
			}
		}
		f.sub.Close()
	}
	if f.pub != nil {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := f.pub.Exec(ctx, "SELECT pg_drop_replication_slot($1)", slot); err == nil ||
				strings.Contains(err.Error(), "does not exist") {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		f.pub.Close()
	}
	for _, db := range []string{subDB, pubDB} {
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)"); err != nil {
			t.Logf("teardown drop %s: %v", db, err)
		}
	}
}

// subscriberSnapshot runs one full collector cycle on the subscriber.
func subscriberSnapshot(t *testing.T, pool *pgxpool.Pool, version int) *collector.Snapshot {
	t.Helper()
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 3600
	cfg.Safety.CPUCeilingPct = 100
	coll := collector.New(pool, cfg, version, noopLog)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { coll.Run(ctx); close(done) }()
	deadline := time.Now().Add(60 * time.Second)
	for coll.LatestSnapshot() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	snap := coll.LatestSnapshot()
	if snap == nil {
		t.Fatal("collector gave no snapshot of the subscriber")
	}
	if !snap.Available("subscriptions") || snap.Subscriptions == nil {
		t.Fatalf("subscriptions not read: unavailable=%v", snap.Unavailable)
	}
	return snap
}

func subscriptionNamed(snap *collector.Snapshot, name string) *collector.SubscriptionInfo {
	for i := range snap.Subscriptions {
		if snap.Subscriptions[i].Name == name {
			return &snap.Subscriptions[i]
		}
	}
	return nil
}

// waitFor polls cond on the subscriber until it holds or 60 s pass.
func waitFor(t *testing.T, p *pgxpool.Pool, what, sql string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		if err := p.QueryRow(context.Background(), sql, args...).Scan(&ok); err == nil && ok {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSubscriptionErrors_ConflictingRowOnRealSubscriber(t *testing.T) {
	f := newSubscriberFixture(t)
	before := subscriberSnapshot(t, f.sub, f.version)
	s := subscriptionNamed(before, f.subName)
	if s == nil || !s.Enabled {
		t.Fatalf("subscription %s = %+v, want it read and enabled", f.subName, s)
	}
	if f.version < 150000 {
		if s.ApplyErrorCount != nil || s.SyncErrorCount != nil || s.Conflicts != nil {
			t.Fatalf("PG14 counters = %+v, want nil (no pg_stat_subscription_stats)", s)
		}
		return
	}
	if s.ApplyErrorCount == nil || *s.ApplyErrorCount != 0 {
		t.Fatalf("apply_error_count before the conflict = %v, want 0", s.ApplyErrorCount)
	}
	if (f.version >= 180000) != (s.Conflicts != nil) {
		t.Fatalf("PG%d conflicts = %v, want them read only from PG18", f.version/10000,
			s.Conflicts)
	}
	now := time.Now()
	ex := subExtras(&now)
	if got := ruleSubscriptionErrors(before, nil, phase2Config(), ex); len(got) != 0 {
		t.Fatalf("before the conflict: %+v, want none", got)
	}
	mustExec(t, f.pub, "INSERT INTO m13_t VALUES (1)")
	waitFor(t, f.sub, "apply_error_count > 0 and the subscription disabled",
		`SELECT ss.apply_error_count > 0 AND NOT s.subenabled
		   FROM pg_stat_subscription_stats ss JOIN pg_subscription s ON s.oid = ss.subid
		  WHERE s.subname = $1`, f.subName)
	after := subscriberSnapshot(t, f.sub, f.version)
	s = subscriptionNamed(after, f.subName)
	if s == nil || s.Enabled || s.ApplyErrorCount == nil || *s.ApplyErrorCount < 1 {
		t.Fatalf("after the conflict: %+v, want disabled with apply errors", s)
	}
	got := findingFor(ruleSubscriptionErrors(after, before, phase2Config(), ex),
		"subscription:"+f.subName)
	if got == nil || got.Category != "subscription_errors" || got.Severity != "critical" ||
		got.RecommendedSQL != "" || got.ActionRisk != "high_risk" {
		t.Fatalf("finding = %+v, want a critical advisory subscription_errors", got)
	}
	if sql, _ := got.Detail["manual_sql"].(string); !strings.Contains(sql, "ENABLE") {
		t.Fatalf("manual_sql = %q, want the ENABLE to run by hand", sql)
	}
}
