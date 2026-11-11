package agentguard

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// GET /api/v1/agents/freezes reads OpenFreezes (agentgov-07): the open
// guard_freezes rows, fleet first, each with its live release request; a
// bounded read of at most MaxListedFreezes rows that says when it was cut.
// The tests use database and principal targets of their own: a fleet flag
// would freeze every agent change other tests make on the server.

func freezeSwitch(t *testing.T, pool *pgxpool.Pool) *Switch {
	t.Helper()
	sw, err := NewSwitch(KillDeps{Store: NewStore(pool), Config: DefaultKillConfig(),
		Targets: func(context.Context) ([]KillTarget, error) { return nil, nil }})
	require.NoError(t, err)
	return sw
}

// openFlag inserts an open freeze, after a kill when killed, and clears it
// when the test ends.
func openFlag(t *testing.T, pool *pgxpool.Pool, scope, target string, killed bool) int64 {
	t.Helper()
	ctx := context.Background()
	var killID *int64
	if killed {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `INSERT INTO sage.guard_kills
			(scope, target, reason, requested_by) VALUES ('database', $1, 'drill', 'a@x')
			RETURNING id`, target).Scan(&id))
		killID = &id
	}
	var id int64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO sage.guard_freezes
		(scope, target, kill_id, reason, set_by) VALUES ($1, $2, $3, 'reason '||$2, 'a@x')
		RETURNING id`, scope, target, killID).Scan(&id))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE sage.guard_freezes
			SET cleared_at = now(), cleared_by = 'test cleanup'
			WHERE id = $1 AND cleared_at IS NULL`, id)
	})
	return id
}

func clearFlag(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE sage.guard_freezes
		SET cleared_at = now(), cleared_by = 'b@x' WHERE id = $1`, id)
	require.NoError(t, err)
}

func releaseRequest(t *testing.T, pool *pgxpool.Pool, freezeID int64, scope, target,
	expiresIn string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(), `INSERT INTO
		sage.guard_unfreeze_requests (scope, target, freeze_id, requested_by,
		requested_by_user, reason, expires_at)
		VALUES ($1, $2, $3, 'first@x', 41, 'closed', now() + $4::interval) RETURNING id`,
		scope, target, freezeID, expiresIn).Scan(&id))
	return id
}

func findFreeze(list FreezeList, scope, target string) *ListedFreeze {
	for i := range list.Items {
		if list.Items[i].Scope == scope && list.Items[i].Target == target {
			return &list.Items[i]
		}
	}
	return nil
}

func TestOpenFreezes_OpenOnlyWithPendingRelease(t *testing.T) {
	pool := livePool(t)
	sw := freezeSwitch(t, pool)
	dbLive, dbOld, dbCleared := uniqName("fz-live"), uniqName("fz-old"), uniqName("fz-gone")
	agent := "agp_" + "fzfzfzfzfzfzfzfzfzfz"
	live := openFlag(t, pool, "database", dbLive, true)
	reqID := releaseRequest(t, pool, live, "database", dbLive, "15 minutes")
	old := openFlag(t, pool, "database", dbOld, false)
	releaseRequest(t, pool, old, "database", dbOld, "-1 minute")
	clearFlag(t, pool, openFlag(t, pool, "database", dbCleared, true))
	openFlag(t, pool, "principal", agent, false)

	list, err := sw.OpenFreezes(context.Background())
	require.NoError(t, err)
	require.False(t, list.Truncated, "a handful of rows is not cut")
	if findFreeze(list, "database", dbCleared) != nil {
		t.Fatalf("a cleared freeze is listed: %+v", list.Items)
	}
	got := findFreeze(list, "database", dbLive)
	require.True(t, got != nil, "open database freeze missing: %+v", list.Items)
	require.Equal(t, "reason "+dbLive, got.Reason)
	require.Equal(t, "a@x", got.SetBy)
	require.True(t, got.AfterKill, "a kill's freeze needs two admins")
	require.False(t, got.SetAt.IsZero(), "set_at")
	require.True(t, got.PendingRelease != nil, "the live request is shown")
	require.Equal(t, reqID, got.PendingRelease.RequestID)
	require.Equal(t, "first@x", got.PendingRelease.RequestedBy)
	require.True(t, got.PendingRelease.ExpiresAt.After(got.PendingRelease.RequestedAt),
		"expires after it was asked")

	stale := findFreeze(list, "database", dbOld)
	require.True(t, stale != nil, "operator database freeze missing")
	require.False(t, stale.AfterKill, "no kill set it")
	if stale.PendingRelease != nil {
		t.Fatalf("an expired request is no request: %+v", stale.PendingRelease)
	}
	p := findFreeze(list, "principal", agent)
	require.True(t, p != nil, "principal freezes are listed too")
}

func TestOpenFreezes_FleetFirst(t *testing.T) {
	pool := livePool(t)
	db := uniqName("fz-order")
	openFlag(t, pool, "database", db, false)
	// The fleet flag is inserted in a transaction the test rolls back, so no
	// other test's agent change ever sees it.
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO sage.guard_freezes (scope, target, reason, set_by)
		VALUES ('fleet', '', 'fleet drill', 'a@x') ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	list, err := listFreezes(ctx, tx)
	require.NoError(t, err)
	require.True(t, len(list.Items) >= 2, "two open flags: %+v", list.Items)
	require.Equal(t, "fleet", list.Items[0].Scope)
	require.True(t, findFreeze(list, "database", db) != nil, "database flag listed")
}

func TestOpenFreezes_AtMostTheLimit(t *testing.T) {
	pool := livePool(t)
	sw := freezeSwitch(t, pool)
	prefix := uniqName("fz-many")
	for i := 0; i < MaxListedFreezes+5; i++ {
		openFlag(t, pool, "database", fmt.Sprintf("%s-%03d", prefix, i), false)
	}
	cleared := prefix + "-cleared"
	clearFlag(t, pool, openFlag(t, pool, "database", cleared, false))
	list, err := sw.OpenFreezes(context.Background())
	require.NoError(t, err)
	require.Equal(t, MaxListedFreezes, len(list.Items))
	require.True(t, list.Truncated, "more open rows than the limit must say so")
	if findFreeze(list, "database", cleared) != nil {
		t.Fatal("a cleared freeze is listed")
	}
}

func TestOpenFreezes_ExactlyTheLimitIsNotTruncated(t *testing.T) {
	// boundary: exactly MaxListedFreezes open rows. One REPEATABLE READ
	// snapshot, so a freeze another package's test commits meanwhile on the
	// shared server cannot push the count past the limit; rolled back.
	pool := livePool(t)
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	var open int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM sage.guard_freezes
		WHERE cleared_at IS NULL`).Scan(&open))
	require.True(t, open <= MaxListedFreezes, "already %d open freezes", open)
	prefix := uniqName("fz-edge")
	for i := open; i < MaxListedFreezes; i++ {
		_, err = tx.Exec(ctx, `INSERT INTO sage.guard_freezes (scope, target, reason,
			set_by) VALUES ('database', $1, 'edge', 'a@x')`, fmt.Sprintf("%s-%03d", prefix, i))
		require.NoError(t, err)
	}
	list, err := listFreezes(ctx, tx)
	require.NoError(t, err)
	require.Equal(t, MaxListedFreezes, len(list.Items))
	require.False(t, list.Truncated, "exactly the limit is the whole list")
}

func TestOpenFreezes_NoControlDatabase(t *testing.T) {
	// nil/empty: a switch without a control database fails closed.
	sw, err := NewSwitch(KillDeps{Config: DefaultKillConfig(),
		Targets: func(context.Context) ([]KillTarget, error) { return nil, nil }})
	require.NoError(t, err)
	list, err := sw.OpenFreezes(context.Background())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	require.Equal(t, 0, len(list.Items))
}

func TestOpenFreezes_ClosedPoolNamesTheRead(t *testing.T) {
	// error propagation: a failed read says what it was reading.
	pool := livePool(t)
	closed, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	require.NoError(t, err)
	closed.Close()
	_, err = freezeSwitch(t, closed).OpenFreezes(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "listing freezes")
}

// No concurrent-access test: OpenFreezes is one read-only SELECT with no
// shared state; the row locks of a release are tested in unfreeze_db_test.
