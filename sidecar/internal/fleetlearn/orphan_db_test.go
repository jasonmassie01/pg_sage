package fleetlearn

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedScope writes a fingerprint and a digest row for scope and, unless
// expiredDaysAgo is negative, a lease that expired that many days ago.
func seedScope(t *testing.T, pool *pgxpool.Pool, scope string, expiredDaysAgo int) {
	t.Helper()
	exec(t, pool, `INSERT INTO sage.fleet_fingerprint (fleet_scope, database_name)
		VALUES ($1, 'db')`, scope)
	exec(t, pool, `INSERT INTO sage.fleet_outcome_digest (fleet_scope, database_name,
		action_class) VALUES ($1, 'db', 'create_index')`, scope)
	if expiredDaysAgo < 0 {
		return
	}
	exec(t, pool, `INSERT INTO sage.fleet_leader_lease (scope, holder, epoch,
		acquired_at, renewed_at, expires_at)
		VALUES ($1, 'gone', 3, now() - interval '30 days', now() - interval '30 days',
		        now() - $2::int * interval '1 day')`, scope, expiredDaysAgo)
}

// scopeRows counts scope's fingerprint, digest and lease rows.
func scopeRows(t *testing.T, pool *pgxpool.Pool, scope string) [3]int {
	t.Helper()
	var n [3]int
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM sage.fleet_fingerprint WHERE fleet_scope = $1),
		(SELECT count(*) FROM sage.fleet_outcome_digest WHERE fleet_scope = $1),
		(SELECT count(*) FROM sage.fleet_leader_lease WHERE scope = $1)`, scope).
		Scan(&n[0], &n[1], &n[2])
	if err != nil {
		t.Fatalf("count rows of %s: %v", scope, err)
	}
	return n
}

func TestStore_PruneOrphanScopesBoundaries(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "orphan_scope")
	seedScope(t, pool, "fleet:old", 15)
	seedScope(t, pool, "fleet:recent", 13)
	seedScope(t, pool, "fleet:nolease", -1)
	seedScope(t, pool, "live", 20) // this store's own scope, however stale
	lease(t, pool, "live", "me", 4, time.Minute)
	st := NewStore(pool, "live")
	got, err := st.PruneOrphanScopes(ctx, Fence{Holder: "me", Epoch: 4}, 14*24*time.Hour)
	if err != nil || len(got) != 1 || got[0] != "fleet:old" {
		t.Fatalf("pruned = %v err=%v, want [fleet:old]", got, err)
	}
	want := map[string][3]int{"fleet:old": {0, 0, 0}, "fleet:recent": {1, 1, 1},
		"fleet:nolease": {1, 1, 0}, "live": {1, 1, 1}}
	for scope, w := range want {
		if n := scopeRows(t, pool, scope); n != w {
			t.Errorf("%s rows (fingerprint, digest, lease) = %v, want %v", scope, n, w)
		}
	}
	again, err := st.PruneOrphanScopes(ctx, Fence{Holder: "me", Epoch: 4}, 14*24*time.Hour)
	if err != nil || len(again) != 0 {
		t.Fatalf("second prune = %v err=%v, want nothing left", again, err)
	}
}

func TestStore_PruneOrphanScopesIsFenced(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "orphan_fenced")
	seedScope(t, pool, "fleet:old", 30)
	lease(t, pool, "live", "successor", 5, time.Minute)
	st := NewStore(pool, "live")
	_, err := st.PruneOrphanScopes(ctx, Fence{Holder: "me", Epoch: 4}, 24*time.Hour)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("stale leader prune err = %v, want ErrFenced", err)
	}
	if n := scopeRows(t, pool, "fleet:old"); n != [3]int{1, 1, 1} {
		t.Fatalf("rows after a fenced prune = %v, want untouched", n)
	}
}

func TestService_RunCyclePrunesOrphanScopes(t *testing.T) {
	ctx := context.Background()
	f := newFleet(t)
	seedScope(t, f.control, "fleet:renamed", 15)
	seedScope(t, f.control, "fleet:recent", 13)
	var logs []string
	sources := f.sources
	svc := NewService(NewStore(f.control, "test-scope"),
		func() []DatabaseSource { return sources }, Settings{},
		func(level, format string, args ...any) { logs = append(logs, level+" "+format) })
	res, err := svc.RunCycle(ctx, Fence{})
	if err != nil || res.Databases != len(sources) {
		t.Fatalf("cycle = %+v err=%v, want every database fingerprinted", res, err)
	}
	if n := scopeRows(t, f.control, "fleet:renamed"); n != [3]int{0, 0, 0} {
		t.Fatalf("renamed scope rows = %v, want pruned at the default 14 days", n)
	}
	if n := scopeRows(t, f.control, "fleet:recent"); n != [3]int{1, 1, 1} {
		t.Fatalf("recent scope rows = %v, want kept", n)
	}
	if got, _ := NewStore(f.control, "test-scope").Fingerprints(ctx); len(got) != len(sources) {
		t.Fatalf("live scope fingerprints = %d, want %d", len(got), len(sources))
	}
	found := false
	for _, l := range logs {
		found = found || strings.HasPrefix(l, "INFO fleet learning: pruned %d orphaned")
	}
	if !found {
		t.Fatalf("logs = %q, want the prune reported", logs)
	}
}

// With election off the prune runs unfenced, and this store's own scope
// can keep a lease row from an earlier run with election on. However long
// ago that lease expired, the live scope's rows are never pruned.
func TestStore_PruneOrphanScopesKeepsOwnScopeWithAStaleLease(t *testing.T) {
	ctx := context.Background()
	pool := freshDB(t, "orphan_own_stale")
	seedScope(t, pool, "live", 40)
	seedScope(t, pool, "fleet:old", 40)
	got, err := NewStore(pool, "live").PruneOrphanScopes(ctx, Fence{}, 14*24*time.Hour)
	if err != nil || len(got) != 1 || got[0] != "fleet:old" {
		t.Fatalf("pruned = %v err=%v, want [fleet:old]", got, err)
	}
	if n := scopeRows(t, pool, "live"); n != [3]int{1, 1, 1} {
		t.Fatalf("own scope rows (fingerprint, digest, lease) = %v, want kept", n)
	}
	if n := scopeRows(t, pool, "fleet:old"); n != [3]int{0, 0, 0} {
		t.Fatalf("orphan rows = %v, want pruned", n)
	}
}
