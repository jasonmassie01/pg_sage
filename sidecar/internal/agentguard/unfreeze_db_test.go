package agentguard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// guard_unfreeze (AGENTDB-SPEC §6.3, §6.10, §6.11; G1-06): widening, so it
// runs through the gate like any other change (emergency stop, trust,
// operator approval). After a kill it needs two admins, neither of them
// the principal's sponsor for the second, unless single-operator mode. It
// restores prior_attrs and rotates every broker credential.

type admin struct {
	id    int
	email string
}

func (f *killFixture) newAdmin(t *testing.T) admin {
	t.Helper()
	id := createUser(t, f.super, "admin")
	return admin{id: id, email: uniqName("admin") + "@example.com"}
}

func unfreezeBy(p Principal, a admin, reason string) UnfreezeRequest {
	return UnfreezeRequest{PrincipalID: p.ID, Reason: reason, Actor: a.email,
		ActorUserID: a.id}
}

func TestUnfreeze_AfterKillTwoPeopleRestoresAndRotates(t *testing.T) {
	f := newKillFixture(t)
	p, oldPassword := f.ensured(t)
	ctx := context.Background()
	_, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	a, b := f.newAdmin(t), f.newAdmin(t)
	sponsor := admin{id: *p.SponsorUserID, email: "sponsor@example.com"}

	res, err := f.sw.Unfreeze(ctx, unfreezeBy(p, a, "incident closed"))
	require.NoError(t, err)
	require.True(t, res.Pending)
	require.False(t, res.Applied)
	require.Equal(t, 2, res.Quorum)
	require.Equal(t, a.email, res.RequestedBy)
	require.Equal(t, StatusFrozen, mustGet(t, f.store, p.ID).Status)
	login, _ := f.attrs(t, p.BrokerRole())
	require.False(t, login, "nothing changes before the second admin")

	again, err := f.sw.Unfreeze(ctx, unfreezeBy(p, a, "incident closed"))
	require.NoError(t, err)
	require.True(t, again.Pending, "the requester cannot be the second person")
	require.Equal(t, res.RequestID, again.RequestID)

	_, err = f.sw.Unfreeze(ctx, unfreezeBy(p, sponsor, "ok"))
	require.ErrorIs(t, err, ErrSponsorCannotApprove)

	since := time.Now().Add(-time.Second)
	done, err := f.sw.Unfreeze(ctx, unfreezeBy(p, b, "agreed"))
	require.NoError(t, err)
	require.True(t, done.Applied)
	require.False(t, done.Pending)
	require.Len(t, done.Clusters, 1)
	require.True(t, done.Clusters[0].Rotated)
	require.Positive(t, done.Clusters[0].ActionID)

	checkUnfrozen(t, f, p, oldPassword, done, b, since)
}

// checkUnfrozen: prior_attrs restored exactly, every credential rotated,
// the control state active and lifted, and the action audited by b.
func checkUnfrozen(t *testing.T, f *killFixture, p Principal, oldPassword string,
	done UnfreezeResult, b admin, since time.Time) {
	t.Helper()
	ctx := context.Background()
	login, limit := f.attrs(t, p.BrokerRole())
	require.True(t, login)
	require.Equal(t, 2, limit)
	login, limit = f.attrs(t, p.LoginRole())
	require.False(t, login)
	require.Equal(t, 5, limit)
	require.True(t, f.loginFails(t, f.dsn, p.BrokerRole(), oldPassword))
	who, newPassword, err := f.brokerLogin(t, p)
	require.NoError(t, err)
	require.Equal(t, p.BrokerRole(), who)
	require.NotEqual(t, oldPassword, newPassword)
	got := mustGet(t, f.store, p.ID)
	require.Equal(t, StatusActive, got.Status)
	require.Empty(t, got.FrozenReason)
	cr, err := f.store.ClusterRolesOf(ctx, p.ID, f.cluster.Key)
	require.NoError(t, err)
	require.Equal(t, RoleStatusActive, cr.Status)
	var open int
	require.NoError(t, f.super.QueryRow(ctx, `SELECT count(*)::int FROM sage.guard_freezes
		WHERE scope = 'principal' AND target = $1 AND cleared_at IS NULL`, p.ID).Scan(&open))
	require.Zero(t, open)
	require.Equal(t, 1, f.actionCount(t, "guard_unfreeze", p, since))
	var approvedBy int
	require.NoError(t, f.super.QueryRow(ctx, `SELECT approved_by FROM sage.action_log
		WHERE id = $1`, done.Clusters[0].ActionID).Scan(&approvedBy))
	require.Equal(t, b.id, approvedBy)
	frozen, _, err := f.sw.Frozen(ctx, p.ID, f.db)
	require.NoError(t, err)
	require.False(t, frozen)
}

func TestUnfreeze_SingleOperatorModeNeedsAReason(t *testing.T) {
	f := newKillFixture(t)
	f.cfg.SingleOperatorMode = true
	f.rebuild(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	_, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	a := f.newAdmin(t)
	_, err = f.sw.Unfreeze(ctx, unfreezeBy(p, a, ""))
	require.ErrorIs(t, err, ErrInvalid)
	res, err := f.sw.Unfreeze(ctx, unfreezeBy(p, a, "on call alone tonight"))
	require.NoError(t, err)
	require.True(t, res.Applied)
	require.True(t, res.SingleOperator)
	require.Equal(t, 1, res.Quorum)
	require.Equal(t, StatusActive, mustGet(t, f.store, p.ID).Status)
}

func TestUnfreeze_PlainFreezeOneAdminStillRotates(t *testing.T) {
	f := newKillFixture(t)
	p, oldPassword := f.ensured(t)
	ctx := context.Background()
	_, err := f.sw.Freeze(ctx, FreezeRequest{PrincipalID: p.ID, Reason: "check",
		Actor: "op@example.com"})
	require.NoError(t, err)
	res, err := f.sw.Unfreeze(ctx, unfreezeBy(p, f.newAdmin(t), ""))
	require.NoError(t, err)
	require.True(t, res.Applied)
	require.Equal(t, 1, res.Quorum)
	require.True(t, f.loginFails(t, f.dsn, p.BrokerRole(), oldPassword))
	_, _, err = f.brokerLogin(t, p)
	require.NoError(t, err)
}

// Unfreeze is not narrowing: the emergency stop and trust level bind it,
// and a refused apply leaves everything frozen with the request pending.
func TestUnfreeze_BlockedByEmergencyStopThenApplies(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	_, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	a, b := f.newAdmin(t), f.newAdmin(t)
	_, err = f.sw.Unfreeze(ctx, unfreezeBy(p, a, "done"))
	require.NoError(t, err)
	f.setRuntime(func(r *policy.RuntimeState) { r.EmergencyStop = true })
	_, err = f.sw.Unfreeze(ctx, unfreezeBy(p, b, "done"))
	var withheld *executor.WithheldError
	require.True(t, errors.As(err, &withheld), "got %v", err)
	require.Equal(t, string(policy.ReasonEmergencyStop), withheld.Decision.BlockedReason)
	require.Equal(t, StatusFrozen, mustGet(t, f.store, p.ID).Status)
	login, _ := f.attrs(t, p.BrokerRole())
	require.False(t, login)

	f.setRuntime(func(r *policy.RuntimeState) {
		r.EmergencyStop = false
		r.TrustLevel = policy.TrustObservation
	})
	_, err = f.sw.Unfreeze(ctx, unfreezeBy(p, b, "done"))
	require.True(t, errors.As(err, &withheld), "observation trust withholds: %v", err)

	f.setRuntime(func(r *policy.RuntimeState) { r.TrustLevel = policy.TrustAdvisory })
	res, err := f.sw.Unfreeze(ctx, unfreezeBy(p, b, "done"))
	require.NoError(t, err)
	require.True(t, res.Applied)
}

func TestUnfreeze_Errors(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	a := f.newAdmin(t)
	_, err := f.sw.Unfreeze(ctx, unfreezeBy(p, a, "x"))
	require.ErrorIs(t, err, ErrNotFrozen)
	ghost := p
	ghost.ID = "agp_qqqqqqqqqqqqqqqqqqqq"
	_, err = f.sw.Unfreeze(ctx, unfreezeBy(ghost, a, "x"))
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.sw.Unfreeze(ctx, UnfreezeRequest{PrincipalID: p.ID, Actor: "a"})
	require.ErrorIs(t, err, ErrApprovalRequired)
	down, err := NewSwitch(KillDeps{Store: NewStore(nil), Config: DefaultKillConfig(),
		Targets: func(context.Context) ([]KillTarget, error) { return f.targets, nil }})
	require.NoError(t, err)
	_, err = down.Unfreeze(ctx, unfreezeBy(p, a, "x"))
	require.ErrorIs(t, err, ErrUnavailable)
}

// A pending request older than the approval TTL is not a second
// signature: the next admin's call starts a new request.
func TestUnfreeze_PendingExpires(t *testing.T) {
	f := newKillFixture(t)
	f.cfg.ApprovalTTL = 200 * time.Millisecond
	f.rebuild(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	_, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	a, b := f.newAdmin(t), f.newAdmin(t)
	first, err := f.sw.Unfreeze(ctx, unfreezeBy(p, a, "x"))
	require.NoError(t, err)
	time.Sleep(400 * time.Millisecond)
	res, err := f.sw.Unfreeze(ctx, unfreezeBy(p, b, "x"))
	require.NoError(t, err)
	require.True(t, res.Pending)
	require.Equal(t, b.email, res.RequestedBy)
	require.NotEqual(t, first.RequestID, res.RequestID)
	require.Equal(t, StatusFrozen, mustGet(t, f.store, p.ID).Status)
}

// A new kill supersedes a pending unfreeze: approvals bind to what the
// approver saw.
func TestUnfreeze_KillSupersedesPending(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	_, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	a, b := f.newAdmin(t), f.newAdmin(t)
	first, err := f.sw.Unfreeze(ctx, unfreezeBy(p, a, "x"))
	require.NoError(t, err)
	_, err = f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	var status string
	require.NoError(t, f.super.QueryRow(ctx, `SELECT status FROM sage.guard_unfreeze_requests
		WHERE id = $1`, first.RequestID).Scan(&status))
	require.Equal(t, "superseded", status)
	res, err := f.sw.Unfreeze(ctx, unfreezeBy(p, b, "x"))
	require.NoError(t, err)
	require.True(t, res.Pending, "b starts a new request; a's was for the old kill")
}

// Two second admins at once: exactly one apply.
func TestUnfreeze_ConcurrentSecondApprovers(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	_, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	_, err = f.sw.Unfreeze(ctx, unfreezeBy(p, f.newAdmin(t), "x"))
	require.NoError(t, err)
	approvers := []admin{f.newAdmin(t), f.newAdmin(t), f.newAdmin(t)}
	since := time.Now().Add(-time.Second)
	var wg sync.WaitGroup
	results := make([]UnfreezeResult, len(approvers))
	errs := make([]error, len(approvers))
	for i, a := range approvers {
		wg.Add(1)
		go func(i int, a admin) {
			defer wg.Done()
			results[i], errs[i] = f.sw.Unfreeze(ctx, unfreezeBy(p, a, "x"))
		}(i, a)
	}
	wg.Wait()
	applied := 0
	for i := range approvers {
		if errs[i] == nil && results[i].Applied {
			applied++
		}
	}
	require.Equal(t, 1, applied)
	require.Equal(t, 1, f.actionCount(t, "guard_unfreeze", p, since))
	require.Equal(t, StatusActive, mustGet(t, f.store, p.ID).Status)
}

// Releasing a fleet or database flag after a kill is widening too: two
// admins. It lifts the flag only; principals stay frozen until each is
// unfrozen.
func TestRelease_FleetFlagTwoPeople(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	_, err := f.sw.Kill(ctx, KillRequest{Scope: KillScopeAll, Reason: "fleet",
		Actor: "admin@example.com"})
	require.NoError(t, err)
	a, b := f.newAdmin(t), f.newAdmin(t)
	rel := ReleaseRequest{Scope: KillScopeAll, Reason: "over", Actor: a.email,
		ActorUserID: a.id}
	res, err := f.sw.Release(ctx, rel)
	require.NoError(t, err)
	require.True(t, res.Pending)
	frozen, _, err := f.sw.Frozen(ctx, p.ID, f.db)
	require.NoError(t, err)
	require.True(t, frozen)
	rel.Actor, rel.ActorUserID = b.email, b.id
	res, err = f.sw.Release(ctx, rel)
	require.NoError(t, err)
	require.True(t, res.Applied)
	frozen, _, err = f.sw.Frozen(ctx, p.ID, f.db)
	require.NoError(t, err)
	require.False(t, frozen)
	require.Equal(t, StatusFrozen, mustGet(t, f.store, p.ID).Status)
	_, err = f.sw.Release(ctx, rel)
	require.ErrorIs(t, err, ErrNotFrozen, "nothing left to release")
}
