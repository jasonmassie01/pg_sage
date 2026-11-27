package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/ha"
)

// The earned-autonomy HA monitor persists its node identity under a key
// that survives restarts and never collides between databases: the
// meta-db record id when there is one, otherwise the instance name.
func TestHAIdentityKey(t *testing.T) {
	id := 7
	if got := haIdentityKey("orders", &id); got != "db:7" {
		t.Fatalf("key with a record id = %q, want db:7", got)
	}
	if got := haIdentityKey("orders", nil); got != "name:orders" {
		t.Fatalf("key by name = %q, want name:orders", got)
	}
	if haIdentityKey("orders", nil) == haIdentityKey("billing", nil) {
		t.Fatal("two databases share a key")
	}
	if got := haIdentityKey("", nil); got != "" {
		t.Fatalf("no database = %q, want no key (persistence off)", got)
	}
}

// fleet-04: the instance monitor carries the HA loop's cadence and the
// failover cooldown (the defaults when unset); pool construction does not
// connect.
func TestNewInstanceHAMonitorAppliesTheCadence(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://sage@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if m := newInstanceHAMonitor(pool, pool, "orders", nil, 20*time.Second,
		45*time.Minute); m == nil || m.Cadence() != 20*time.Second ||
		m.FailoverCooldown() != 45*time.Minute {
		t.Fatalf("persisted monitor = %v, want one at a 20s cadence, 45m cooldown", m)
	}
	if m := newInstanceHAMonitor(pool, nil, "orders", nil, 0, 0); m == nil ||
		m.Cadence() != ha.DefaultCheckInterval ||
		m.FailoverCooldown() != ha.DefaultFailoverCooldown {
		t.Fatalf("unpersisted monitor = %v, want the default cadence and cooldown", m)
	}
	m := newInstanceHAMonitor(pool, nil, "orders", nil, time.Minute, time.Minute)
	if s := m.Snapshot(); s.Role != ha.RoleUnknown {
		t.Fatalf("an unchecked instance monitor = %q, want unknown", s.Role)
	}
}
