package upkeep

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func ensuresOf(f *fakeRoles, id string) []agentguard.RoleRequest {
	ensures, _ := f.calls()
	var out []agentguard.RoleRequest
	for _, r := range ensures {
		if r.PrincipalID == id {
			out = append(out, r)
		}
	}
	return out
}

func TestRotateBroker_DueRolesUnderTheLastEnsureApproval(t *testing.T) {
	pool := livePool(t)
	sponsor := createUser(t, pool, "admin")
	first, latest := createUser(t, pool, "operator"), createUser(t, pool, "admin")
	key := uniqKey()
	due, fresh := newPrincipal(t, pool, sponsor), newPrincipal(t, pool, sponsor)
	registerRoles(t, pool, due, key, agentguard.RoleStatusActive, week+time.Hour)
	registerRoles(t, pool, fresh, key, agentguard.RoleStatusActive, week-time.Hour)
	ensureLogged(t, pool, due, key, first, 0, 20*24*time.Hour)
	actionID := ensureLogged(t, pool, due, key, latest, 77, 8*24*time.Hour)
	ensureLogged(t, pool, due, uniqKey(), sponsor, 0, time.Hour) // another cluster
	ensureLogged(t, pool, fresh, key, latest, 0, time.Hour)
	roles := &fakeRoles{}
	rep, err := runner(t, pool, roles, weekly(), target(pool, key)).
		RotateBroker(context.Background(), Fence{})
	require.NoError(t, err)

	got := ensuresOf(roles, due.ID)
	require.Equal(t, 1, len(got))
	req := got[0]
	require.True(t, req.RotateCredential, "a rotation sets a new broker password")
	require.Equal(t, key, req.Cluster.Key)
	require.Equal(t, latest, req.Approval.ApprovedBy)
	require.Equal(t, int64(77), req.Approval.ApprovalID)
	require.True(t, req.Scheduled != nil)
	require.Equal(t, JobBrokerRotation, req.Scheduled.Job)
	require.Equal(t, latest, req.Scheduled.OriginalApprovedBy)
	require.Equal(t, actionID, req.Scheduled.OriginalActionID)
	require.Equal(t, int64(77), req.Scheduled.OriginalApprovalID)
	_, ok := outcomeFor(rep.Done, due.ID)
	require.True(t, ok, "%+v", rep)
	require.Equal(t, 0, len(ensuresOf(roles, fresh.ID)), "rotated within the period")
}

func TestRotateBroker_SkipsFrozenKilledAndRetired(t *testing.T) {
	pool := livePool(t)
	sponsor := createUser(t, pool, "admin")
	key := uniqKey()
	frozen, killed, retired := newPrincipal(t, pool, sponsor), newPrincipal(t, pool, sponsor),
		newPrincipal(t, pool, sponsor)
	registerRoles(t, pool, frozen, key, agentguard.RoleStatusActive, 2*week)
	registerRoles(t, pool, killed, key, agentguard.RoleStatusKilled, 2*week)
	registerRoles(t, pool, retired, key, agentguard.RoleStatusActive, 2*week)
	for _, p := range []agentguard.Principal{frozen, killed, retired} {
		ensureLogged(t, pool, p, key, sponsor, 0, 2*week)
	}
	ctx := context.Background()
	store := agentguard.NewStore(pool)
	_, err := store.SetStatus(ctx, frozen.ID, agentguard.StatusFrozen, "incident")
	require.NoError(t, err)
	retireAgo(t, pool, retired, sponsor, time.Hour)
	roles := &fakeRoles{}
	_, err = runner(t, pool, roles, weekly(), target(pool, key)).RotateBroker(ctx, Fence{})
	require.NoError(t, err)
	for _, p := range []agentguard.Principal{frozen, killed, retired} {
		require.Equal(t, 0, len(ensuresOf(roles, p.ID)),
			"rotation never re-asserts the roles of "+p.Name)
	}
}

func TestRotateBroker_WithoutAnActiveApproverReportsAndSkips(t *testing.T) {
	pool := livePool(t)
	sponsor := createUser(t, pool, "admin")
	gone, demoted := createUser(t, pool, "operator"), createUser(t, pool, "operator")
	key := uniqKey()
	none, deleted, viewer := newPrincipal(t, pool, sponsor), newPrincipal(t, pool, sponsor),
		newPrincipal(t, pool, sponsor)
	for _, p := range []agentguard.Principal{none, deleted, viewer} {
		registerRoles(t, pool, p, key, agentguard.RoleStatusActive, 2*week)
	}
	ensureLogged(t, pool, deleted, key, gone, 0, 2*week)
	ensureLogged(t, pool, viewer, key, demoted, 0, 2*week)
	ctx := context.Background()
	_, err := pool.Exec(ctx, "DELETE FROM sage.users WHERE id = $1", gone)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE sage.users SET role = 'viewer' WHERE id = $1", demoted)
	require.NoError(t, err)
	roles := &fakeRoles{}
	rep, err := runner(t, pool, roles, weekly(), target(pool, key)).RotateBroker(ctx, Fence{})
	require.NoError(t, err)
	want := map[string]string{none.ID: ReasonNoApprover, deleted.ID: ReasonApproverInactive,
		viewer.ID: ReasonApproverInactive}
	for id, reason := range want {
		o, ok := outcomeFor(rep.Skipped, id)
		require.True(t, ok, "%s must be skipped: %+v", id, rep)
		require.Equal(t, reason, o.Reason)
		require.Equal(t, 0, len(ensuresOf(roles, id)), "never another user's approval")
		_, _, _, open := findingOf(t, pool, UpkeepFindingCategory, JobBrokerRotation+":"+id)
		require.True(t, open, "a skipped rotation is reported as a finding")
	}
}

func TestRotateBroker_SuccessResolvesTheSkipFinding(t *testing.T) {
	pool := livePool(t)
	sponsor := createUser(t, pool, "admin")
	key := uniqKey()
	p := newPrincipal(t, pool, sponsor)
	registerRoles(t, pool, p, key, agentguard.RoleStatusActive, 2*week)
	roles := &fakeRoles{}
	r := runner(t, pool, roles, weekly(), target(pool, key))
	ctx := context.Background()
	_, err := r.RotateBroker(ctx, Fence{})
	require.NoError(t, err)
	ident := JobBrokerRotation + ":" + p.ID
	require.Equal(t, 1, findingCount(t, pool, UpkeepFindingCategory, ident, "open"))

	ensureLogged(t, pool, p, key, sponsor, 0, time.Hour)
	rep, err := r.RotateBroker(ctx, Fence{})
	require.NoError(t, err)
	_, ok := outcomeFor(rep.Done, p.ID)
	require.True(t, ok, "%+v", rep)
	require.Equal(t, 0, findingCount(t, pool, UpkeepFindingCategory, ident, "open"))
}

func TestRotateBroker_FencedOff(t *testing.T) {
	pool := livePool(t)
	sponsor := createUser(t, pool, "admin")
	key := uniqKey()
	p := newPrincipal(t, pool, sponsor)
	registerRoles(t, pool, p, key, agentguard.RoleStatusActive, 2*week)
	ensureLogged(t, pool, p, key, sponsor, 0, 2*week)
	roles := &fakeRoles{}
	fence := takeLease(t, pool, uniq("scope"))
	fence.Holder = "someone-else"
	_, err := runner(t, pool, roles, weekly(), target(pool, key)).
		RotateBroker(context.Background(), fence)
	require.ErrorIs(t, err, ErrFenced)
	require.Equal(t, 0, len(ensuresOf(roles, p.ID)))
}

func TestRotateBroker_ExpiredLeaseIsFenced(t *testing.T) {
	pool := livePool(t)
	fence := takeLease(t, pool, uniq("scope"))
	_, err := pool.Exec(context.Background(), `UPDATE sage.fleet_leader_lease
		SET expires_at = now() - interval '1 second' WHERE scope = $1`, fence.Scope)
	require.NoError(t, err)
	_, err = runner(t, pool, &fakeRoles{}, weekly()).
		RotateBroker(context.Background(), fence)
	require.ErrorIs(t, err, ErrFenced)
}

// Two passes at once (a lease handover race) neither panic nor race; each
// reports the due principal (the role contract itself is idempotent and
// serialized by its role lock).
func TestRotateBroker_ConcurrentPasses(t *testing.T) {
	pool := livePool(t)
	sponsor := createUser(t, pool, "admin")
	key := uniqKey()
	p := newPrincipal(t, pool, sponsor)
	registerRoles(t, pool, p, key, agentguard.RoleStatusActive, 2*week)
	ensureLogged(t, pool, p, key, sponsor, 0, 2*week)
	roles := &fakeRoles{}
	r := runner(t, pool, roles, weekly(), target(pool, key))
	var wg sync.WaitGroup
	reps := make([]Report, 4)
	errs := make([]error, 4)
	for i := range reps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reps[i], errs[i] = r.RotateBroker(context.Background(), Fence{})
		}(i)
	}
	wg.Wait()
	for i := range reps {
		require.NoError(t, errs[i])
		_, ok := outcomeFor(reps[i].Done, p.ID)
		require.True(t, ok, "pass %d: %+v", i, reps[i])
	}
	require.Equal(t, 4, len(ensuresOf(roles, p.ID)))
}

// Core records an ensure in the database that administered the cluster,
// which may differ between ensures: the newest across the cluster's
// databases is the approval a rotation carries, whatever the order.
func TestRotateBroker_NewestEnsureAcrossTheClustersDatabases(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	other, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "upkeep-second"))
	require.NoError(t, err)
	t.Cleanup(other.Close)
	require.NoError(t, schema.Bootstrap(ctx, other))
	sponsor := createUser(t, pool, "admin")
	older, newer := createUser(t, pool, "operator"), createUser(t, pool, "operator")
	key := uniqKey()
	p := newPrincipal(t, pool, sponsor)
	registerRoles(t, pool, p, key, agentguard.RoleStatusActive, 2*week)
	for _, order := range [][2]string{{"a", "b"}, {"b", "a"}} {
		roles := &fakeRoles{}
		_, err := pool.Exec(ctx, "DELETE FROM sage.action_log WHERE principal_id = $1", p.ID)
		require.NoError(t, err)
		_, err = other.Exec(ctx, "DELETE FROM sage.action_log WHERE principal_id = $1", p.ID)
		require.NoError(t, err)
		ensureLogged(t, pool, p, key, older, 0, 10*24*time.Hour)
		newest := ensureLogged(t, other, p, key, newer, 0, 9*24*time.Hour)
		first := agentguard.KillTarget{Name: order[0], Pool: pool, ClusterKey: key,
			Executor: fakeApplier{"x"}}
		second := agentguard.KillTarget{Name: order[1], Pool: other, ClusterKey: key}
		_, err = runner(t, pool, roles, weekly(), first, second).RotateBroker(ctx, Fence{})
		require.NoError(t, err)
		got := ensuresOf(roles, p.ID)
		require.Equal(t, 1, len(got), "order %v", order)
		require.Equal(t, newer, got[0].Approval.ApprovedBy, "order %v", order)
		require.Equal(t, newest, got[0].Scheduled.OriginalActionID, "order %v", order)
	}
}
