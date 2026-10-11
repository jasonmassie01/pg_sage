package upkeep

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// outcomeFor finds p's outcome in one of a report's lists.
func outcomeFor(list []Outcome, id string) (Outcome, bool) {
	for _, o := range list {
		if o.PrincipalID == id {
			return o, true
		}
	}
	return Outcome{}, false
}

func retiresOf(f *fakeRoles, id string) []agentguard.RoleRequest {
	_, retires := f.calls()
	var out []agentguard.RoleRequest
	for _, r := range retires {
		if r.PrincipalID == id {
			out = append(out, r)
		}
	}
	return out
}

func TestDropRetired_AfterGraceUnderTheRetiringAdmin(t *testing.T) {
	pool := livePool(t)
	admin := createUser(t, pool, "admin")
	key := uniqKey()
	due, early, active, done := newPrincipal(t, pool, admin), newPrincipal(t, pool, admin),
		newPrincipal(t, pool, admin), newPrincipal(t, pool, admin)
	for _, p := range []agentguard.Principal{due, early, active} {
		registerRoles(t, pool, p, key, agentguard.RoleStatusActive, time.Hour)
	}
	registerRoles(t, pool, done, key, agentguard.RoleStatusRetired, time.Hour)
	retireAgo(t, pool, due, admin, week+time.Hour)
	retireAgo(t, pool, early, admin, week-time.Hour)
	retireAgo(t, pool, done, admin, week+time.Hour)
	roles := &fakeRoles{}
	rep, err := runner(t, pool, roles, weekly(), target(pool, key)).
		DropRetired(context.Background(), Fence{})
	require.NoError(t, err)

	got := retiresOf(roles, due.ID)
	require.Equal(t, 1, len(got))
	req := got[0]
	require.Equal(t, key, req.Cluster.Key)
	require.Equal(t, admin, req.Approval.ApprovedBy)
	require.True(t, req.Executor != nil, "the cluster's executor runs the retire")
	require.True(t, req.Scheduled != nil, "a scheduled retire says so in the audit")
	require.Equal(t, JobRetireGrace, req.Scheduled.Job)
	require.Equal(t, admin, req.Scheduled.OriginalApprovedBy)
	o, ok := outcomeFor(rep.Done, due.ID)
	require.True(t, ok, "the due principal is reported done: %+v", rep)
	require.Equal(t, int64(201), o.ActionID)
	for _, p := range []agentguard.Principal{early, active, done} {
		require.Equal(t, 0, len(retiresOf(roles, p.ID)), p.Name)
	}
}

func TestDropRetired_ZeroGraceDropsAtTheNextPass(t *testing.T) {
	pool := livePool(t)
	admin := createUser(t, pool, "admin")
	key := uniqKey()
	p := newPrincipal(t, pool, admin)
	registerRoles(t, pool, p, key, agentguard.RoleStatusKilled, time.Hour)
	retireAgo(t, pool, p, admin, time.Second)
	roles := &fakeRoles{}
	cfg := Config{RetireGrace: 0, Rotation: week, Batch: 1000}
	_, err := runner(t, pool, roles, cfg, target(pool, key)).
		DropRetired(context.Background(), Fence{})
	require.NoError(t, err)
	require.Equal(t, 1, len(retiresOf(roles, p.ID)), "killed roles are dropped too")
}

func TestDropRetired_PagesThroughEveryDuePrincipal(t *testing.T) {
	pool := livePool(t)
	admin := createUser(t, pool, "admin")
	key := uniqKey()
	var ps []agentguard.Principal
	for i := 0; i < 3; i++ {
		p := newPrincipal(t, pool, admin)
		registerRoles(t, pool, p, key, agentguard.RoleStatusActive, time.Hour)
		retireAgo(t, pool, p, admin, week+time.Duration(i+1)*time.Hour)
		ps = append(ps, p)
	}
	roles := &fakeRoles{}
	cfg := Config{RetireGrace: week, Rotation: week, Batch: 1}
	_, err := runner(t, pool, roles, cfg, target(pool, key)).
		DropRetired(context.Background(), Fence{})
	require.NoError(t, err)
	for _, p := range ps {
		require.Equal(t, 1, len(retiresOf(roles, p.ID)), "page size 1 still reaches "+p.Name)
	}
}

func TestDropRetired_WithoutAnApproverReportsAndSkips(t *testing.T) {
	pool := livePool(t)
	admin := createUser(t, pool, "admin")
	gone := createUser(t, pool, "admin")
	demoted := createUser(t, pool, "admin")
	key := uniqKey()
	none, deleted, viewer := newPrincipal(t, pool, admin), newPrincipal(t, pool, admin),
		newPrincipal(t, pool, admin)
	for _, p := range []agentguard.Principal{none, deleted, viewer} {
		registerRoles(t, pool, p, key, agentguard.RoleStatusActive, time.Hour)
	}
	retireAgo(t, pool, none, 0, week+time.Hour)
	retireAgo(t, pool, deleted, gone, week+time.Hour)
	retireAgo(t, pool, viewer, demoted, week+time.Hour)
	ctx := context.Background()
	_, err := pool.Exec(ctx, "DELETE FROM sage.users WHERE id = $1", gone)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE sage.users SET role = 'viewer' WHERE id = $1", demoted)
	require.NoError(t, err)
	roles := &fakeRoles{}
	rep, err := runner(t, pool, roles, weekly(), target(pool, key)).DropRetired(ctx, Fence{})
	require.NoError(t, err)
	want := map[string]string{none.ID: ReasonNoApprover, deleted.ID: ReasonNoApprover,
		viewer.ID: ReasonApproverInactive}
	for id, reason := range want {
		o, ok := outcomeFor(rep.Skipped, id)
		require.True(t, ok, "%s must be skipped: %+v", id, rep)
		require.Equal(t, reason, o.Reason)
		require.True(t, o.Detail != "", "a skip says why")
		require.Equal(t, 0, len(retiresOf(roles, id)), "never another user's approval")
		sev, title, _, open := findingOf(t, pool, UpkeepFindingCategory,
			JobRetireGrace+":"+id)
		require.True(t, open, "a skipped retire is reported as a finding")
		require.Equal(t, "warning", sev)
		require.Contains(t, title, id)
	}
}

func TestDropRetired_RevokeIncompleteIsReportedNeverForced(t *testing.T) {
	pool := livePool(t)
	admin := createUser(t, pool, "admin")
	key := uniqKey()
	p := newPrincipal(t, pool, admin)
	registerRoles(t, pool, p, key, agentguard.RoleStatusActive, time.Hour)
	retireAgo(t, pool, p, admin, week+time.Hour)
	fix := "REVOKE SELECT ON public.t FROM sage_agentb_x GRANTED BY owner"
	roles := &fakeRoles{err: fmt.Errorf("%w: %w", agentguard.ErrPostCheck,
		&agentguard.DeniedError{Reason: agentguard.ReasonRevokeIncomplete,
			Detail: "privileges from another grantor remain", Fix: fix})}
	r := runner(t, pool, roles, weekly(), target(pool, key))
	ctx := context.Background()
	rep, err := r.DropRetired(ctx, Fence{})
	require.NoError(t, err)
	o, ok := outcomeFor(rep.Incomplete, p.ID)
	require.True(t, ok, "revoke_incomplete is its own outcome: %+v", rep)
	require.Equal(t, fix, o.Fix)
	_, _, sql, open := findingOf(t, pool, UpkeepFindingCategory, JobRetireGrace+":"+p.ID)
	require.True(t, open, "the residue is reported as a finding")
	require.Equal(t, fix, sql)

	// The other grantor revokes; the next pass drops the roles and resolves.
	roles.err = nil
	rep, err = r.DropRetired(ctx, Fence{})
	require.NoError(t, err)
	_, ok = outcomeFor(rep.Done, p.ID)
	require.True(t, ok, "%+v", rep)
	require.Equal(t, 0, findingCount(t, pool, UpkeepFindingCategory,
		JobRetireGrace+":"+p.ID, "open"))
	require.Equal(t, 1, findingCount(t, pool, UpkeepFindingCategory,
		JobRetireGrace+":"+p.ID, "resolved"))
}

func TestDropRetired_WithheldAndUnreachableAndFailed(t *testing.T) {
	pool := livePool(t)
	admin := createUser(t, pool, "admin")
	key, lost := uniqKey(), uniqKey()
	p, q := newPrincipal(t, pool, admin), newPrincipal(t, pool, admin)
	registerRoles(t, pool, p, key, agentguard.RoleStatusActive, time.Hour)
	registerRoles(t, pool, q, lost, agentguard.RoleStatusActive, time.Hour)
	retireAgo(t, pool, p, admin, week+time.Hour)
	retireAgo(t, pool, q, admin, week+time.Hour)
	roles := &fakeRoles{err: &executor.WithheldError{Decision: executor.ActionPolicyDecision{
		BlockedReason: "trust_level"}}}
	ctx := context.Background()
	r := runner(t, pool, roles, weekly(), target(pool, key))
	rep, err := r.DropRetired(ctx, Fence{})
	require.NoError(t, err)
	o, ok := outcomeFor(rep.Withheld, p.ID)
	require.True(t, ok, "%+v", rep)
	require.Equal(t, "trust_level", o.Reason)
	o, ok = outcomeFor(rep.Skipped, q.ID)
	require.True(t, ok, "a cluster with no monitored database is skipped: %+v", rep)
	require.Equal(t, ReasonClusterUnreachable, o.Reason)
	require.Equal(t, 0, len(retiresOf(roles, q.ID)))

	roles.err = errors.New("connection reset by peer")
	rep, err = r.DropRetired(ctx, Fence{})
	require.NoError(t, err)
	o, ok = outcomeFor(rep.Failed, p.ID)
	require.True(t, ok, "%+v", rep)
	require.Contains(t, o.Detail, "connection reset by peer")
}

func TestDropRetired_FencedOffWritesNothing(t *testing.T) {
	pool := livePool(t)
	admin := createUser(t, pool, "admin")
	key := uniqKey()
	p := newPrincipal(t, pool, admin)
	registerRoles(t, pool, p, key, agentguard.RoleStatusActive, time.Hour)
	retireAgo(t, pool, p, admin, week+time.Hour)
	roles := &fakeRoles{}
	r := runner(t, pool, roles, weekly(), target(pool, key))
	ctx := context.Background()
	scope := uniq("scope")
	lease := takeLease(t, pool, scope)

	stale := lease
	stale.Epoch = 2 // a lease this sidecar held before another took it
	_, err := r.DropRetired(ctx, stale)
	require.ErrorIs(t, err, ErrFenced)
	require.Equal(t, 0, len(retiresOf(roles, p.ID)), "a fenced-off leader drops nothing")

	_, err = r.DropRetired(ctx, lease)
	require.NoError(t, err)
	require.Equal(t, 1, len(retiresOf(roles, p.ID)), "the current leader drops it")
}

func TestDropRetired_ControlDatabaseDown(t *testing.T) {
	pool := livePool(t)
	r := runner(t, pool, &fakeRoles{}, weekly())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.DropRetired(ctx, Fence{})
	require.True(t, err != nil, "an unreadable control database is an error")
	require.Contains(t, err.Error(), "retired")
}

func TestRecordRetiringAdmin(t *testing.T) {
	pool := livePool(t)
	admin := createUser(t, pool, "admin")
	p := newPrincipal(t, pool, admin)
	ctx := context.Background()
	require.NoError(t, RecordRetiringAdmin(ctx, pool, p.ID, admin))
	var by *int
	require.NoError(t, pool.QueryRow(ctx, `SELECT retired_by FROM sage.guard_principals
		WHERE id = $1`, p.ID).Scan(&by))
	require.True(t, by != nil && *by == admin, "retired_by = %v", by)

	require.ErrorIs(t, RecordRetiringAdmin(ctx, pool, p.ID, 0), ErrInvalid)
	require.ErrorIs(t, RecordRetiringAdmin(ctx, pool, "nope", admin), agentguard.ErrNotFound)
	require.ErrorIs(t, RecordRetiringAdmin(ctx, pool, "agp_aaaaaaaaaaaaaaaaaaaa", admin),
		agentguard.ErrNotFound)
	require.ErrorIs(t, RecordRetiringAdmin(ctx, nil, p.ID, admin), ErrInvalid)

	// A retired principal keeps the admin who retired it.
	other := createUser(t, pool, "admin")
	_, err := agentguard.NewStore(pool).SetStatus(ctx, p.ID, agentguard.StatusRetired, "x")
	require.NoError(t, err)
	require.ErrorIs(t, RecordRetiringAdmin(ctx, pool, p.ID, other), agentguard.ErrRetired)
	require.NoError(t, pool.QueryRow(ctx, `SELECT retired_by FROM sage.guard_principals
		WHERE id = $1`, p.ID).Scan(&by))
	require.Equal(t, admin, *by)
}
