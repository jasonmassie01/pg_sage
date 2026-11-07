package alerting

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// fleetDatabase is one monitored database with its own Manager, sharing a
// PagerDuty routing key with its siblings.
type fleetDatabase struct {
	name    string
	pool    *pgxpool.Pool
	manager *Manager
	finding int64
}

func connectExtraAlertDB(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect extra database %s: %v", label, err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap extra database %s: %v", label, err)
	}
	return pool
}

func insertSameIdentity(t *testing.T, pool *pgxpool.Pool, category string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO sage.findings (category, severity, title, status,
		   last_seen, object_identifier, detail)
		 VALUES ($1, 'critical', 'bloat on public.orders', 'open', now(),
		   'public.orders', '{}') RETURNING id`, category).Scan(&id)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	return id
}

func newFleetDatabase(
	t *testing.T, name string, pool *pgxpool.Pool, ch Channel, category string,
) *fleetDatabase {
	t.Helper()
	start := dbNow(t, pool)
	m := New(pool, ManagerConfig{DatabaseName: name},
		map[string][]Channel{"critical": {ch}}, noop)
	m.lastCheck = start.Add(-time.Second)
	return &fleetDatabase{name: name, pool: pool, manager: m,
		finding: insertSameIdentity(t, pool, category)}
}

func alertLogKeys(t *testing.T, db *fleetDatabase, status string) []string {
	t.Helper()
	rows, err := db.pool.Query(context.Background(),
		`SELECT dedup_key FROM sage.alert_log
		  WHERE finding_id = $1 AND status = $2 ORDER BY id`, db.finding, status)
	if err != nil {
		t.Fatalf("query alert_log of %s: %v", db.name, err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan alert_log of %s: %v", db.name, err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate alert_log of %s: %v", db.name, err)
	}
	return keys
}

// eventsFor returns the captured events of this test's category.
func eventsFor(tr *captureTransport, category string) []pdEvent {
	var out []pdEvent
	for _, ev := range tr.snapshot() {
		if strings.Contains(ev.DedupKey, category+":") {
			out = append(out, ev)
		}
	}
	return out
}

func evaluateAll(t *testing.T, dbs ...*fleetDatabase) {
	t.Helper()
	for _, db := range dbs {
		if err := db.manager.evaluate(context.Background()); err != nil {
			t.Fatalf("evaluate %s: %v", db.name, err)
		}
	}
}

// TestFleetDatabasesKeepSeparatePagesAndThrottles: two databases with the
// same finding identity share one PagerDuty routing key. Each gets its own
// page, alert_log row and throttle state; resolving one closes only its page.
func TestFleetDatabasesKeepSeparatePagesAndThrottles(t *testing.T) {
	ordersPool := connectAlertTestDB(t)
	defer ordersPool.Close()
	billingPool := connectExtraAlertDB(t, "alerting02_billing")
	category := fmt.Sprintf("alerting02_%d", time.Now().UnixNano())
	ch, tr := capturingPagerDuty()
	orders := newFleetDatabase(t, "orders", ordersPool, ch, category)
	billing := newFleetDatabase(t, "billing", billingPool, ch, category)

	evaluateAll(t, orders, billing)
	identity := category + ":public.orders"
	got := eventsFor(tr, category)
	if len(got) != 2 || got[0].DedupKey != "orders:"+identity ||
		got[1].DedupKey != "billing:"+identity {
		t.Fatalf("triggers = %+v, want one per database", got)
	}
	for _, db := range []*fleetDatabase{orders, billing} {
		if keys := alertLogKeys(t, db, "sent"); len(keys) != 1 || keys[0] != identity {
			t.Errorf("%s alert_log sent keys = %v, want [%s]", db.name, keys, identity)
		}
		if d := db.manager.throttle.Decide(identity, "critical"); d != DecisionThrottled {
			t.Errorf("%s throttle decision = %v, want throttled", db.name, d)
		}
	}

	if _, err := ordersPool.Exec(context.Background(), `UPDATE sage.findings
		SET status = 'resolved', resolved_at = now() WHERE id = $1`,
		orders.finding); err != nil {
		t.Fatalf("resolve orders finding: %v", err)
	}
	evaluateAll(t, orders, billing)
	got = eventsFor(tr, category)
	if len(got) != 3 || got[2].EventAction != "resolve" ||
		got[2].DedupKey != "orders:"+identity {
		t.Fatalf("events after resolve = %+v, want one orders resolve", got)
	}
	if n := len(alertLogKeys(t, orders, "resolved")); n != 1 {
		t.Errorf("orders resolved rows = %d, want 1", n)
	}
	if n := len(alertLogKeys(t, billing, "resolved")); n != 0 {
		t.Errorf("billing resolved rows = %d, want 0", n)
	}
}

// TestFleetThrottleSeedStaysPerDatabase: after a restart each Manager
// seeds its throttle from its own database's alert_log only.
func TestFleetThrottleSeedStaysPerDatabase(t *testing.T) {
	ordersPool := connectAlertTestDB(t)
	defer ordersPool.Close()
	billingPool := connectExtraAlertDB(t, "alerting02_seed")
	category := fmt.Sprintf("alerting02s_%d", time.Now().UnixNano())
	ch, tr := capturingPagerDuty()
	orders := newFleetDatabase(t, "orders", ordersPool, ch, category)
	evaluateAll(t, orders)
	identity := category + ":public.orders"

	restartedOrders := New(ordersPool, ManagerConfig{DatabaseName: "orders"}, nil, noop)
	restartedOrders.seedThrottle(context.Background())
	if d := restartedOrders.throttle.Decide(identity, "critical"); d != DecisionThrottled {
		t.Errorf("restarted orders manager decision = %v, want throttled", d)
	}
	billing := newFleetDatabase(t, "billing", billingPool, ch, category)
	billing.manager.seedThrottle(context.Background())
	if d := billing.manager.throttle.Decide(identity, "critical"); d != DecisionSend {
		t.Fatalf("billing inherited orders' throttle: decision = %v", d)
	}
	evaluateAll(t, billing)
	if got := eventsFor(tr, category); len(got) != 2 ||
		got[1].DedupKey != "billing:"+identity {
		t.Fatalf("events = %+v, want orders then billing trigger", got)
	}
}
