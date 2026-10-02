package ha

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The persisted HA identity against real PostgreSQL: sage.ha_identity
// round-trips every field (unknown ones as NULL), keys are isolated, and
// a live monitor that finds a different persisted timeline at startup
// opens the failover cooldown.

func storePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test:%s:%d", t.Name(), time.Now().UnixNano())
}

func TestPostgresIdentityStore_RoundTrip(t *testing.T) {
	pool, ctx := storePool(t)
	s := NewPostgresIdentityStore(pool)
	key := uniqueKey(t)
	if _, found, err := s.LoadIdentity(ctx, key); err != nil || found {
		t.Fatalf("missing key: found %v (%v)", found, err)
	}
	at := time.Date(2026, 10, 2, 3, 4, 5, 123456000, time.UTC)
	want := Persisted{Identity: Identity{Role: RoleReplica, TimelineID: 4,
		SystemID: "7692103199828676646", StartedAt: at.Add(-time.Hour)},
		LastChange: at.Add(-time.Minute), ObservedAt: at}
	if err := s.SaveIdentity(ctx, key, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, found, err := s.LoadIdentity(ctx, key)
	if err != nil || !found || !samePersisted(got, want) {
		t.Fatalf("loaded %+v (%v %v), want %+v", got, found, err, want)
	}
	// Upsert: the row is replaced, unknown fields become unknown.
	next := Persisted{Identity: Identity{Role: RolePrimary}, ObservedAt: at.Add(time.Hour)}
	if err := s.SaveIdentity(ctx, key, next); err != nil {
		t.Fatalf("save again: %v", err)
	}
	got, _, err = s.LoadIdentity(ctx, key)
	if err != nil || !samePersisted(got, next) || got.Identity.TimelineID != 0 ||
		got.Identity.SystemID != "" || !got.LastChange.IsZero() {
		t.Fatalf("after upsert %+v (%v), want %+v", got, err, next)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sage.ha_identity WHERE monitor_key = $1",
		key).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d rows for the key (%v), want 1", n, err)
	}
}

func TestPostgresIdentityStore_RejectsInvalidInput(t *testing.T) {
	pool, ctx := storePool(t)
	s := NewPostgresIdentityStore(pool)
	primaryOnly := Persisted{Identity: Identity{Role: RolePrimary}}
	if err := s.SaveIdentity(ctx, "", primaryOnly); err == nil {
		t.Fatal("an empty key was saved")
	}
	if err := s.SaveIdentity(ctx, uniqueKey(t), Persisted{Identity: Identity{
		Role: RoleUnknown}}); err == nil {
		t.Fatal("an unknown role was saved: only an observed role is history")
	}
	var nilStore *PostgresIdentityStore
	if _, _, err := nilStore.LoadIdentity(ctx, "x"); err == nil {
		t.Fatal("a nil store loaded")
	}
}

func samePersisted(a, b Persisted) bool {
	return a.Identity.Role == b.Identity.Role && a.Identity.TimelineID == b.Identity.TimelineID &&
		a.Identity.SystemID == b.Identity.SystemID &&
		a.Identity.StartedAt.Equal(b.Identity.StartedAt) && a.LastChange.Equal(b.LastChange) &&
		a.ObservedAt.Equal(b.ObservedAt)
}

// A live monitor persists the server's real identity, a restarted monitor
// with the same history sees no change, and one whose history names
// another timeline (a failover while it was down) opens the cooldown.
func TestLiveMonitor_PersistsAndComparesTheIdentity(t *testing.T) {
	pool, ctx := storePool(t)
	store := NewPostgresIdentityStore(pool)
	key := uniqueKey(t)
	first := New(pool, noopLog).WithIdentityStore(store, key)
	if first.Check(ctx) {
		t.Fatal("the test primary reported as not-primary")
	}
	var sys string
	var tli int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT system_identifier::text
		FROM pg_control_system()),
		('x' || substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8))::bit(32)::int8`).
		Scan(&sys, &tli); err != nil {
		t.Fatalf("identity: %v", err)
	}
	p, found, err := store.LoadIdentity(ctx, key)
	if err != nil || !found || p.Identity.Role != RolePrimary || p.Identity.SystemID != sys ||
		p.Identity.TimelineID != tli || p.Identity.StartedAt.IsZero() {
		t.Fatalf("persisted %+v (%v %v), want primary %s timeline %d", p, found, err, sys, tli)
	}
	again := New(pool, noopLog).WithIdentityStore(store, key)
	again.Check(ctx)
	if !again.LastRoleChange().IsZero() {
		t.Fatalf("a restart with the same identity opened the cooldown: %v",
			again.LastRoleChange())
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.ha_identity SET timeline_id = timeline_id + 1
		WHERE monitor_key = $1`, key); err != nil {
		t.Fatalf("simulate an earlier timeline: %v", err)
	}
	before := time.Now()
	after := New(pool, noopLog).WithIdentityStore(store, key)
	after.Check(ctx)
	if got := after.LastRoleChange(); got.Before(before) {
		t.Fatalf("a timeline change while down did not open the cooldown: %v", got)
	}
}
