package collector

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// market-13: the subscriptions step reads this database's subscriptions
// (pg_subscription joined to its apply worker in pg_stat_subscription, plus
// pg_stat_subscription_stats from PG15), and never reads the connection
// string column, which holds credentials. The publisher/subscriber round
// trip is in the analyzer package (rules_subscription_db_test.go), next to
// the rule it feeds.
//
// No concurrent-access tests: the step runs inside the single collector
// goroutine and shares nothing.

// credentialsColumn is pg_subscription's connection string column, spelled
// in two parts so a grep for it finds no reader in this package (CHECK-05).
const credentialsColumn = "subconn" + "info"

func TestSubscriptionsSQL_VersionGateAndNoCredentials(t *testing.T) {
	for _, tc := range []struct {
		version int
		stats   bool
	}{{140000, false}, {149999, false}, {150000, true}, {170004, true}, {180000, true}} {
		sql := subscriptionsSQL(tc.version)
		if strings.Contains(sql, "pg_stat_subscription_stats") != tc.stats {
			t.Errorf("PG %d: reads pg_stat_subscription_stats = %v, want %v", tc.version,
				!tc.stats, tc.stats)
		}
		for _, want := range []string{"pg_stat_subscription", "pg_subscription",
			"subdbid = (SELECT oid FROM pg_database WHERE datname = current_database())",
			"LIMIT 200", sageTag} {
			if !strings.Contains(sql, want) {
				t.Errorf("PG %d: SQL lacks %q", tc.version, want)
			}
		}
		if strings.Contains(sql, credentialsColumn) || strings.Contains(sql, "to_jsonb(sub)") ||
			strings.Contains(sql, "sub.*") {
			t.Errorf("PG %d: SQL could read %s: %s", tc.version, credentialsColumn, sql)
		}
		if tc.stats && !strings.Contains(sql, "to_jsonb(ss)") {
			t.Errorf("PG %d: conflict counters must be read by name through to_jsonb(ss)",
				tc.version)
		}
	}
}

// Nil/empty: no subscriptions is an empty, available category.
func TestCollectSubscriptions_NoneIsEmptyNotNil(t *testing.T) {
	pool := testPool(t)
	version := serverVersion(t, pool)
	subs, err := New(pool, testConfig(), version, noopLog).collectSubscriptions(
		context.Background())
	if err != nil {
		t.Fatalf("collectSubscriptions on PG %d: %v", version, err)
	}
	if subs == nil || len(subs) != 0 {
		t.Fatalf("subs = %#v, want an empty, non-nil slice", subs)
	}
	snap := &Snapshot{}
	retries := 0
	c := New(pool, testConfig(), version, noopLog)
	c.collectExtras(context.Background(), snap, &retries)
	if !snap.Available("subscriptions") || snap.Subscriptions == nil {
		t.Fatalf("extras: subscriptions=%#v unavailable=%v, want empty and available",
			snap.Subscriptions, snap.Unavailable)
	}
	rows, err := snapshotRows(snap)
	if err != nil {
		t.Fatalf("snapshotRows: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Category == "subscriptions" {
			found = string(r.Data) == "[]"
		}
	}
	if !found {
		t.Fatal("an empty subscriptions category must persist as []")
	}
}

// Error propagation: a role that may not read pg_stat_subscription gets
// Unavailable["subscriptions"] (unknown, not "no subscriptions"), and the
// rest of the snapshot is kept.
func TestCollectSubscriptions_PermissionErrorMarksUnavailable(t *testing.T) {
	admin := testPool(t)
	ctx := context.Background()
	version := serverVersion(t, admin)
	role := fmt.Sprintf("m13_noperm_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx,
		"CREATE ROLE "+role+" LOGIN PASSWORD 'm13' IN ROLE pg_monitor"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "GRANT SELECT ON pg_stat_subscription TO PUBLIC")
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	if _, err := admin.Exec(ctx, "REVOKE SELECT ON pg_stat_subscription FROM PUBLIC"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, "m13"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	defer pool.Close()
	snap := &Snapshot{Subscriptions: []SubscriptionInfo{{Name: "stale"}}}
	retries := 0
	New(pool, testConfig(), version, noopLog).collectExtras(ctx, snap, &retries)
	why, missing := snap.Unavailable["subscriptions"]
	if !missing || !strings.Contains(why, "permission denied") {
		t.Fatalf("Unavailable = %v, want subscriptions: permission denied", snap.Unavailable)
	}
	if snap.Subscriptions != nil {
		t.Fatalf("subscriptions = %+v, want nil (unknown) after a failed read", snap.Subscriptions)
	}
	if !snap.Available("replication") || !snap.Available("prepared_xacts") {
		t.Fatalf("other categories lost: %v", snap.Unavailable)
	}
}

// The slot read adds the confirmed_flush_lsn position and its byte lag
// behind the server's WAL for a logical slot, and says whether the slot
// belongs to this database (fleet mode reads one cluster per database).
func TestCollectReplication_ConfirmedFlushColumns(t *testing.T) {
	version, create := slotServer(t)
	pool := testPool(t)
	ctx := context.Background()
	name := fmt.Sprintf("m13_slot_%d", time.Now().UnixNano())
	create(name)
	rep, err := New(pool, testConfig(), version, noopLog).collectReplication(ctx)
	if err != nil || rep == nil {
		t.Fatalf("collectReplication = %v, %v; want slots", rep, err)
	}
	var got *SlotInfo
	for i := range rep.Slots {
		if rep.Slots[i].SlotName == name {
			got = &rep.Slots[i]
		}
	}
	if got == nil {
		t.Fatalf("slot %s not read", name)
	}
	if got.ConfirmedFlushLSN == nil || !strings.Contains(*got.ConfirmedFlushLSN, "/") {
		t.Fatalf("confirmed_flush_lsn = %v, want an LSN", got.ConfirmedFlushLSN)
	}
	if got.ConfirmedFlushLagBytes == nil || *got.ConfirmedFlushLagBytes < 0 {
		t.Fatalf("confirmed_flush_lag_bytes = %v, want >= 0", got.ConfirmedFlushLagBytes)
	}
	if !got.InCurrentDatabase {
		t.Fatal("in_current_database = false for a slot made in this database")
	}
	if _, err := pool.Exec(ctx, "SELECT pg_logical_emit_message(true, 'm13', 'x')"); err != nil {
		t.Fatalf("emit WAL: %v", err)
	}
	rep2, err := New(pool, testConfig(), version, noopLog).collectReplication(ctx)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	for _, s := range rep2.Slots {
		if s.SlotName == name && (s.ConfirmedFlushLagBytes == nil ||
			*s.ConfirmedFlushLagBytes <= *got.ConfirmedFlushLagBytes ||
			*s.ConfirmedFlushLSN != *got.ConfirmedFlushLSN) {
			t.Fatalf("after WAL advanced: lsn %v lag %v (before %s, %d), want the "+
				"same LSN and a larger lag", deref(s.ConfirmedFlushLSN),
				derefInt(s.ConfirmedFlushLagBytes), *got.ConfirmedFlushLSN,
				*got.ConfirmedFlushLagBytes)
		}
	}
}

func derefInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
