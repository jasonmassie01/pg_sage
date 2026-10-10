package grants

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// guard_revoke and the expiry reconciler (§6.3, §6.6, G1-08, G1-08b,
// G1-09): a revoke runs GRANTED BY the recorded grantor, leaves no residue
// for (grantee, grantor), is audited, and is narrowing: it runs during an
// emergency stop and at trust.level observation.

func (f *fixture) grantID(t *testing.T) Grant {
	t.Helper()
	res, err := f.manager.Grant(context.Background(), f.request("id"))
	require.NoError(t, err)
	return relationGrant(t, res.Grants)
}

func (f *fixture) row(t *testing.T, id int64) Grant {
	t.Helper()
	g, err := Get(context.Background(), f.super, id)
	require.NoError(t, err)
	return g
}

func TestRevoke_OperatorRevokesGrantedByAndLeavesNoResidue(t *testing.T) {
	f := newFixture(t)
	g := f.grantID(t)
	res, err := f.manager.Revoke(context.Background(), RevokeRequest{Target: f.target,
		GrantID: g.ID, Cause: CauseOperator, ApprovedBy: 1})
	require.NoError(t, err)
	require.True(t, res.ActionID > 0)
	require.Empty(t, res.Residue)
	require.False(t, f.can(t, f.p.BrokerRole(), "id"), "privilege gone")
	require.False(t, f.schemaUsage(t, f.p.BrokerRole()), "schema USAGE went with the last")
	got := f.row(t, g.ID)
	require.Equal(t, StateRevoked, got.State)
	require.NotNil(t, got.RevokedAt)
	require.Equal(t, res.ActionID, *got.RevokeActionID)
	require.NotNil(t, res.Schema)
	require.Equal(t, StateRevoked, f.row(t, res.Schema.ID).State)
	sqls := f.actions(t, executor.ActionTypeGuardRevoke)
	require.Len(t, sqls, 1)
	grantedBy := "GRANTED BY " + pgx.Identifier{f.adminRol}.Sanitize()
	require.True(t, strings.Contains(sqls[0], grantedBy), sqls[0])
	require.True(t, strings.Contains(sqls[0], "REVOKE USAGE ON SCHEMA"), sqls[0])
	residue, err := agentguard.ForeignGrants(context.Background(), f.admin,
		[]string{f.p.BrokerRole()})
	require.NoError(t, err)
	require.Empty(t, residue)
	_, err = f.manager.Revoke(context.Background(), RevokeRequest{Target: f.target,
		GrantID: g.ID, Cause: CauseOperator, ApprovedBy: 1})
	require.ErrorIs(t, err, ErrNotActive)
}

func TestRevoke_SchemaUsageStaysWhileAnotherGrantUsesIt(t *testing.T) {
	f := newFixture(t)
	a, b := f.grantID(t), f.grantID(t)
	_, err := f.manager.Revoke(context.Background(), RevokeRequest{Target: f.target,
		GrantID: a.ID, Cause: CauseOperator, ApprovedBy: 1})
	require.NoError(t, err)
	require.True(t, f.schemaUsage(t, f.p.BrokerRole()), "b still needs USAGE")
	require.True(t, f.can(t, f.p.BrokerRole(), "id"), "b still holds id")
	_, err = f.manager.Revoke(context.Background(), RevokeRequest{Target: f.target,
		GrantID: b.ID, Cause: CauseOperator, ApprovedBy: 1})
	require.NoError(t, err)
	require.False(t, f.schemaUsage(t, f.p.BrokerRole()))
	require.False(t, f.can(t, f.p.BrokerRole(), "id"))
}

func TestRevoke_InvalidAndUnknown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.manager.Revoke(ctx, RevokeRequest{Target: f.target, GrantID: 0,
		Cause: CauseOperator})
	require.ErrorIs(t, err, agentguard.ErrInvalid)
	_, err = f.manager.Revoke(ctx, RevokeRequest{Target: f.target, GrantID: 1 << 40,
		Cause: CauseOperator})
	require.ErrorIs(t, err, agentguard.ErrNotFound)
	g := f.grantID(t)
	_, err = f.manager.Revoke(ctx, RevokeRequest{Target: f.target, GrantID: g.ID,
		Cause: "because"})
	require.ErrorIs(t, err, agentguard.ErrInvalid)
	bad := f.target
	bad.Executor = nil
	_, err = f.manager.Revoke(ctx, RevokeRequest{Target: bad, GrantID: g.ID,
		Cause: CauseExpired})
	require.ErrorIs(t, err, agentguard.ErrInvalid)
	require.True(t, f.can(t, f.p.BrokerRole(), "id"), "nothing revoked")
}

// G1-09: residue from another grantor marks the grant revoke_incomplete
// and D10 then denies the object.
func TestRevoke_ForeignGrantorResidueIsIncompleteAndDenied(t *testing.T) {
	f := newFixture(t)
	g := f.grantID(t)
	table := pgx.Identifier{f.schema, "orders"}.Sanitize()
	f.exec1(t, "SET ROLE "+f.owner+"; GRANT SELECT (id) ON "+table+" TO "+
		f.p.BrokerRole()+"; RESET ROLE")
	res, err := f.manager.Revoke(context.Background(), RevokeRequest{Target: f.target,
		GrantID: g.ID, Cause: CauseOperator, ApprovedBy: 1})
	require.NoError(t, err)
	require.Len(t, res.Residue, 1)
	require.Equal(t, f.owner, res.Residue[0].Grantor)
	got := f.row(t, g.ID)
	require.Equal(t, StateRevokeIncomplete, got.State)
	require.True(t, strings.Contains(got.RevokeDetail, "other grantor "+f.owner),
		got.RevokeDetail)
	c := f.checker()
	ok, err := c.LeaseActive(context.Background(), f.p.ID, itoa(g.ID), f.db)
	require.NoError(t, err)
	require.False(t, ok)
	g2 := f.grantID(t) // a fresh grant on the same object is still denied
	ok, err = c.LeaseActive(context.Background(), f.p.ID, itoa(g2.ID), f.db)
	require.NoError(t, err)
	require.False(t, ok, "residue on the object denies every grant of it")
	err = c.CheckObjects(context.Background(), f.principalNow(t), f.db, f.target.Env,
		objects(f.schema, "id"))
	denied(t, err, "agent_lease_expired")
	// The other grantor removes it; the reconciler's re-check completes it.
	f.exec1(t, "SET ROLE "+f.owner+"; REVOKE SELECT (id) ON "+table+" FROM "+
		f.p.BrokerRole()+"; RESET ROLE")
	rep, err := f.manager.ExpireDue(context.Background(), f.target, Fence{}, 100)
	require.NoError(t, err)
	require.Contains(t, rep.Completed, g.ID)
	require.Equal(t, StateRevoked, f.row(t, g.ID).State)
}

// G1-08: a grant expires on schedule: the reconciler revokes it, audited,
// GRANTED BY the recorded grantor, with no residue.
func TestExpireDue_RevokesExpiredGrants(t *testing.T) {
	f := newFixture(t)
	due, live := f.grantID(t), f.grantID(t)
	f.expireNow(t, due.ID)
	rep, err := f.manager.ExpireDue(context.Background(), f.target, Fence{}, 100)
	require.NoError(t, err)
	require.Equal(t, []int64{due.ID}, rep.Revoked)
	require.Empty(t, rep.Failed)
	require.Equal(t, StateRevoked, f.row(t, due.ID).State)
	require.Equal(t, StateActive, f.row(t, live.ID).State)
	require.True(t, f.can(t, f.p.BrokerRole(), "id"), "live grant still holds")
	f.expireNow(t, live.ID)
	rep, err = f.manager.ExpireDue(context.Background(), f.target, Fence{}, 100)
	require.NoError(t, err)
	require.Equal(t, []int64{live.ID}, rep.Revoked)
	require.False(t, f.can(t, f.p.BrokerRole(), "id"))
	require.False(t, f.schemaUsage(t, f.p.BrokerRole()))
	// The first revoke ran no statement: the live grant still listed id, and
	// PostgreSQL keeps one ACL entry per column. The last one revoked it.
	sqls := f.actions(t, executor.ActionTypeGuardRevoke)
	require.Len(t, sqls, 2)
	require.Equal(t, "", sqls[0])
	require.True(t, strings.Contains(sqls[1], "GRANTED BY "+
		pgx.Identifier{f.adminRol}.Sanitize()), sqls[1])
	var causes []string
	rows, err := f.super.Query(context.Background(), `SELECT after_state->>'cause'
		FROM sage.action_log WHERE principal_id = $1 AND action_type = 'guard_revoke'`, f.p.ID)
	require.NoError(t, err)
	causes, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	require.Equal(t, []string{CauseExpired, CauseExpired}, causes)
	rep, err = f.manager.ExpireDue(context.Background(), f.target, Fence{}, 100)
	require.NoError(t, err)
	require.Empty(t, rep.Revoked, "nothing left to do")
}

// G1-08b: the same with emergency_stop on, the executor disabled and
// trust.level observation, because expiry narrows.
func TestExpireDue_RunsDuringStopAndAtObservation(t *testing.T) {
	f := newFixture(t)
	g := f.grantID(t)
	f.setRuntime(func(r *policy.RuntimeState) {
		r.EmergencyStop, r.ExecutorEnabled = true, false
		r.TrustLevel = policy.TrustObservation
	})
	f.expireNow(t, g.ID)
	rep, err := f.manager.ExpireDue(context.Background(), f.target, Fence{}, 100)
	require.NoError(t, err)
	require.Equal(t, []int64{g.ID}, rep.Revoked)
	require.False(t, f.can(t, f.p.BrokerRole(), "id"))
	var cause string
	require.NoError(t, f.super.QueryRow(context.Background(), `SELECT after_state->>'cause'
		FROM sage.action_log WHERE principal_id = $1 AND action_type = 'guard_revoke'`,
		f.p.ID).Scan(&cause))
	require.Equal(t, CauseExpired, cause)
	req, err := gateRequest(executor.ActionTypeGuardRevoke, f.p.ID, f.target, nil, nil, false)
	require.NoError(t, err)
	d := f.exec.StandingPolicyGate().(policy.Explainer).Explain(context.Background(), req)
	require.Equal(t, policy.VerdictExecute, d.Verdict)
	require.Equal(t, policy.ReasonNarrowingDuringStop, d.Reason)
}

func TestExpireDue_LimitBoundsOnePass(t *testing.T) {
	f := newFixture(t)
	a, b, c := f.grantID(t), f.grantID(t), f.grantID(t)
	f.expireNow(t, a.ID, b.ID, c.ID)
	rep, err := f.manager.ExpireDue(context.Background(), f.target, Fence{}, 2)
	require.NoError(t, err)
	require.Len(t, rep.Revoked, 2)
	rep, err = f.manager.ExpireDue(context.Background(), f.target, Fence{}, 2)
	require.NoError(t, err)
	require.Len(t, rep.Revoked, 1)
	_, err = f.manager.ExpireDue(context.Background(), f.target, Fence{}, 0)
	require.ErrorIs(t, err, agentguard.ErrInvalid)
}

// Two reconcilers racing (a stale leader and the new one): each grant is
// revoked once, with one audit row.
func TestExpireDue_ConcurrentPassesRevokeOnce(t *testing.T) {
	f := newFixture(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		ids = append(ids, f.grantID(t).ID)
	}
	f.expireNow(t, ids...)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.manager.ExpireDue(context.Background(), f.target, Fence{}, 100)
			if err != nil {
				t.Errorf("pass: %v", err)
			}
		}()
	}
	wg.Wait()
	require.Len(t, f.actions(t, executor.ActionTypeGuardRevoke), 3)
	for _, id := range ids {
		require.Equal(t, StateRevoked, f.row(t, id).State)
	}
}

// G1-08 failover: leader A stops renewing; within one reconcile interval
// plus one lease TTL, B leads and revokes. A's stale fence writes nothing.
func TestExpireDue_LeaderFailoverWithTwoElectors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const ttl, interval = 2 * time.Second, 500 * time.Millisecond
	scope := uniq("g1g-scope")
	store := leader.NewPostgresStore(f.super)
	a := leader.NewElector(store, scope, "sidecar-a", ttl)
	b := leader.NewElector(store, scope, "sidecar-b", ttl)
	require.NoError(t, a.Tick(ctx))
	require.NoError(t, b.Tick(ctx))
	require.True(t, a.IsLeader() && !b.IsLeader(), "A leads first")
	fenceOf := func(e *leader.Elector) (Fence, bool) {
		holder, epoch, ok := e.Fence()
		return Fence{Control: f.super, Scope: scope, Holder: holder, Epoch: epoch}, ok
	}
	staleA, ok := fenceOf(a)
	require.True(t, ok)
	g := f.grantID(t)
	f.expireNow(t, g.ID)
	// A dies here (no more ticks). B's loop: tick, then reconcile if leader.
	start := time.Now()
	deadline := start.Add(interval + ttl + 2*time.Second)
	for time.Now().Before(deadline) && f.row(t, g.ID).State == StateActive {
		require.NoError(t, b.Tick(ctx))
		if fb, ok := fenceOf(b); ok {
			_, err := f.manager.ExpireDue(ctx, f.target, fb, 100)
			require.NoError(t, err)
		}
		time.Sleep(interval)
	}
	elapsed := time.Since(start)
	t.Logf("revoked %v after the expiry; bound interval+ttl = %v", elapsed, interval+ttl)
	require.Equal(t, StateRevoked, f.row(t, g.ID).State)
	require.False(t, f.can(t, f.p.BrokerRole(), "id"))
	require.True(t, elapsed <= interval+ttl+time.Second, "revoked after %v", elapsed)
	g2 := f.grantID(t)
	f.expireNow(t, g2.ID)
	_, err := f.manager.ExpireDue(ctx, f.target, staleA, 100)
	require.True(t, errors.Is(err, ErrFenced), "stale leader fenced: %v", err)
	require.Equal(t, StateActive, f.row(t, g2.ID).State)
	require.True(t, f.can(t, f.p.BrokerRole(), "id"))
}

func TestExpireDue_UnknownFenceScopeIsFenced(t *testing.T) {
	f := newFixture(t)
	g := f.grantID(t)
	f.expireNow(t, g.ID)
	_, err := f.manager.ExpireDue(context.Background(), f.target, Fence{Control: f.super,
		Scope: uniq("nobody"), Holder: "x", Epoch: 1}, 100)
	require.ErrorIs(t, err, ErrFenced)
	require.Equal(t, StateActive, f.row(t, g.ID).State)
	_, err = f.manager.ExpireDue(context.Background(), f.target, Fence{Scope: "s",
		Holder: "x", Epoch: 1}, 100)
	require.ErrorIs(t, err, agentguard.ErrInvalid, "a fence needs its control pool")
}

// A retired principal's role is gone with its privileges: the reconciler
// closes its rows instead of failing every pass.
func TestExpireDue_DroppedRoleClosesRows(t *testing.T) {
	f := newFixture(t)
	g := f.grantID(t)
	// As a retire does: pg_sage's role (ADMIN on the agent role) revokes
	// what it granted, then drops the role, outside the grant registry.
	b := f.p.BrokerRole()
	for _, stmt := range []string{
		"REVOKE SELECT (id) ON TABLE " + pgx.Identifier{f.schema, "orders"}.Sanitize() +
			" FROM " + b,
		"REVOKE USAGE ON SCHEMA " + pgx.Identifier{f.schema}.Sanitize() + " FROM " + b,
		"REVOKE CONNECT ON DATABASE " + pgx.Identifier{f.db}.Sanitize() + " FROM " + b,
		"DROP ROLE " + b} {
		_, err := f.admin.Exec(context.Background(), stmt)
		require.NoError(t, err, stmt)
	}
	var exists bool
	require.NoError(t, f.super.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 "+
		"FROM pg_roles WHERE rolname = $1)", f.p.BrokerRole()).Scan(&exists))
	require.False(t, exists, "broker role dropped")
	_, err := f.super.Exec(context.Background(), `UPDATE sage.guard_grants
		SET expires_at = now() - interval '1 second', granted_at = now() - interval '2 minutes'
		WHERE principal_id = $1`, f.p.ID)
	require.NoError(t, err)
	rep, err := f.manager.ExpireDue(context.Background(), f.target, Fence{}, 100)
	require.NoError(t, err)
	require.Empty(t, rep.Failed)
	require.Equal(t, []int64{g.ID}, rep.Revoked)
	got := f.row(t, g.ID)
	require.Equal(t, StateRevoked, got.State)
	require.Equal(t, "the role was dropped", got.RevokeDetail)
	page, err := List(context.Background(), f.super, Filter{PrincipalID: f.p.ID,
		State: StateActive, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, page.Items, "the schema row closed too")
}
