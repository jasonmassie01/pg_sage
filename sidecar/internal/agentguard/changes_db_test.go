package agentguard

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-06 on real PostgreSQL: a widening change waits as one pending
// row per principal until a second admin, who is neither the requester
// nor the sponsor, confirms the identical patch within the TTL.

type changeFixture struct {
	pool             *pgxpool.Pool
	s                *Store
	p                Principal
	sponsor, a, b, c int
}

// newChangeFixture is a stage principal sponsored by an admin, plus two
// other admins (a requests, b and c confirm).
func newChangeFixture(t *testing.T) changeFixture {
	t.Helper()
	pool := livePool(t)
	s := NewStore(pool)
	f := changeFixture{pool: pool, s: s, sponsor: createUser(t, pool, "admin"),
		a: createUser(t, pool, "admin"), b: createUser(t, pool, "admin"),
		c: createUser(t, pool, "admin")}
	p, err := s.Create(context.Background(), CreateRequest{Name: uniqName("widen"),
		SponsorUserID: &f.sponsor, Profile: "readonly-analyst", EnvCeiling: EnvStage,
		CreatedBy: "admin@example.com"})
	require.NoError(t, err)
	f.p = p
	return f
}

func (f changeFixture) request(t *testing.T, by int, patch Patch) PendingChange {
	t.Helper()
	pc, err := f.s.RequestChange(context.Background(), ChangeRequest{PrincipalID: f.p.ID,
		Patch: patch, Actor: fmt.Sprintf("admin-%d@example.com", by), TTL: time.Minute,
		Audit: Audit{ActorUserID: by, SourceIP: "192.0.2.1", Reason: "reads prod"}})
	require.NoError(t, err)
	return pc
}

func (f changeFixture) confirm(by int, id int64, patch Patch) (Principal, error) {
	return f.s.ConfirmChange(context.Background(), ChangeConfirm{PrincipalID: f.p.ID,
		ChangeID: id, Patch: patch, Actor: fmt.Sprintf("admin-%d@example.com", by),
		Audit: Audit{ActorUserID: by, SourceIP: "192.0.2.2"}})
}

func changeStatus(t *testing.T, pool *pgxpool.Pool, id int64) (string, *int) {
	t.Helper()
	var status string
	var decidedBy *int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT status, decided_by_user
		FROM sage.guard_principal_changes WHERE id = $1`, id).Scan(&status, &decidedBy))
	return status, decidedBy
}

func TestChange_RequestThenSecondAdminApplies(t *testing.T) {
	f := newChangeFixture(t)
	prod := Patch{EnvCeiling: envPtr(EnvProd)}
	pc := f.request(t, f.a, prod)
	require.Positive(t, pc.ID)
	require.Equal(t, "pending", pc.Status)
	require.Equal(t, f.a, pc.RequestedByUser)
	require.True(t, pc.ExpiresAt.After(time.Now().Add(30*time.Second)), "%v", pc.ExpiresAt)
	require.Equal(t, EnvStage, mustGet(t, f.s, f.p.ID).EnvCeiling, "nothing applied yet")
	pending, err := f.s.PendingChanges(context.Background(), f.p.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, pc.ID, pending[0].ID)
	require.Equal(t, "prod", string(*pending[0].Patch.EnvCeiling))

	got, err := f.confirm(f.b, pc.ID, prod)
	require.NoError(t, err)
	require.Equal(t, EnvProd, got.EnvCeiling)
	status, by := changeStatus(t, f.pool, pc.ID)
	require.Equal(t, "applied", status)
	require.NotNil(t, by)
	require.Equal(t, f.b, *by)
	pending, err = f.s.PendingChanges(context.Background(), f.p.ID)
	require.NoError(t, err)
	require.Len(t, pending, 0, "an applied change is no longer pending")

	ev := govEvents(t, f.pool, f.p.ID)
	require.Len(t, ev, 3, "created, requested, confirmed: %+v", ev)
	require.Equal(t, auth.AuditAgentChangeRequested, ev[1].Event)
	require.Equal(t, f.a, ev[1].Actor)
	require.Equal(t, float64(pc.ID), ev[1].Detail["change_id"])
	require.Equal(t, "stage", sub(t, ev[1].Detail, "before")["env_ceiling"])
	require.Equal(t, "prod", sub(t, ev[1].Detail, "after")["env_ceiling"])
	require.Equal(t, auth.AuditAgentChangeConfirmed, ev[2].Event)
	require.Equal(t, f.b, ev[2].Actor)
	require.Equal(t, f.sponsor, ev[2].Target)
	require.Equal(t, float64(pc.ID), ev[2].Detail["change_id"])
	require.Equal(t, float64(f.a), ev[2].Detail["requested_by_user"])
	require.Equal(t, "prod", sub(t, ev[2].Detail, "after")["env_ceiling"])
	requireChained(t, f.pool, ev[2].ID)
}

func TestChange_ConfirmRefusals(t *testing.T) {
	f := newChangeFixture(t)
	prod := Patch{EnvCeiling: envPtr(EnvProd)}
	pc := f.request(t, f.a, prod)
	_, err := f.confirm(f.a, pc.ID, prod)
	require.ErrorIs(t, err, ErrSecondAdminRequired, "the requester")
	_, err = f.confirm(f.sponsor, pc.ID, prod)
	require.ErrorIs(t, err, ErrSponsorCannotApprove, "the sponsor")
	_, err = f.confirm(f.b, pc.ID, Patch{EnvCeiling: envPtr(EnvDev)})
	require.ErrorIs(t, err, ErrChangeMismatch, "another patch")
	_, err = f.confirm(f.b, pc.ID, Patch{EnvCeiling: envPtr(EnvProd),
		Profile: profilePtr("readonly-analyst")})
	require.ErrorIs(t, err, ErrChangeMismatch, "an added field")
	_, err = f.confirm(f.b, pc.ID+1_000_000, prod)
	require.ErrorIs(t, err, ErrChangeNotFound)
	other := newPrincipal(t, f.s, &f.sponsor)
	_, err = f.s.ConfirmChange(context.Background(), ChangeConfirm{PrincipalID: other.ID,
		ChangeID: pc.ID, Patch: prod, Actor: "b@example.com",
		Audit: Audit{ActorUserID: f.b}})
	require.ErrorIs(t, err, ErrChangeNotFound, "a change of another principal")
	status, _ := changeStatus(t, f.pool, pc.ID)
	require.Equal(t, "pending", status, "refusals leave the request pending")
	require.Equal(t, EnvStage, mustGet(t, f.s, f.p.ID).EnvCeiling)

	// A patch that hands the sponsorship to the confirmer is refused too.
	move := Patch{EnvCeiling: envPtr(EnvProd), SponsorUserID: &f.c}
	pc = f.request(t, f.a, move)
	_, err = f.confirm(f.c, pc.ID, move)
	require.ErrorIs(t, err, ErrSponsorCannotApprove, "the incoming sponsor")
	got, err := f.confirm(f.b, pc.ID, move)
	require.NoError(t, err)
	require.Equal(t, f.c, *got.SponsorUserID)
	require.Equal(t, EnvProd, got.EnvCeiling)
}

func TestChange_ExpiredIsMarkedAndRefused(t *testing.T) {
	f := newChangeFixture(t)
	prod := Patch{EnvCeiling: envPtr(EnvProd)}
	pc := f.request(t, f.a, prod)
	_, err := f.pool.Exec(context.Background(), `UPDATE sage.guard_principal_changes
		SET expires_at = now() - interval '1 second' WHERE id = $1`, pc.ID)
	require.NoError(t, err)
	_, err = f.confirm(f.b, pc.ID, prod)
	require.ErrorIs(t, err, ErrChangeExpired)
	status, _ := changeStatus(t, f.pool, pc.ID)
	require.Equal(t, "expired", status, "the expiry is kept although confirm failed")
	_, err = f.confirm(f.b, pc.ID, prod)
	require.ErrorIs(t, err, ErrChangeNotPending, "an expired change stays closed")
	pending, err := f.s.PendingChanges(context.Background(), f.p.ID)
	require.NoError(t, err)
	require.Len(t, pending, 0)
	require.Equal(t, EnvStage, mustGet(t, f.s, f.p.ID).EnvCeiling)
}

func TestChange_SecondRequestSupersedesTheFirst(t *testing.T) {
	f := newChangeFixture(t)
	first := f.request(t, f.a, Patch{EnvCeiling: envPtr(EnvProd)})
	second := f.request(t, f.b, Patch{Profile: profilePtr("app-writer")})
	require.Equal(t, first.ID, second.SupersededID)
	status, _ := changeStatus(t, f.pool, first.ID)
	require.Equal(t, "superseded", status)
	_, err := f.confirm(f.c, first.ID, Patch{EnvCeiling: envPtr(EnvProd)})
	require.ErrorIs(t, err, ErrChangeNotPending)
	got, err := f.confirm(f.a, second.ID, Patch{Profile: profilePtr("app-writer")})
	require.NoError(t, err, "a, who asked for the first, may confirm b's second")
	require.Equal(t, "app-writer", got.Profile)
	require.Equal(t, EnvStage, got.EnvCeiling, "the superseded ceiling never applied")
}

func TestChange_ConcurrentConfirmsApplyOnce(t *testing.T) {
	f := newChangeFixture(t)
	prod := Patch{EnvCeiling: envPtr(EnvProd)}
	pc := f.request(t, f.a, prod)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, by := range []int{f.b, f.c} {
		wg.Add(1)
		go func(i, by int) {
			defer wg.Done()
			_, errs[i] = f.confirm(by, pc.ID, prod)
		}(i, by)
	}
	wg.Wait()
	ok, closed := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrChangeNotPending):
			closed++
		default:
			t.Fatalf("unexpected confirm error: %v", err)
		}
	}
	require.Equal(t, 1, ok, "exactly one confirm applies")
	require.Equal(t, 1, closed, "the other finds it applied")
	confirmed := 0
	for _, ev := range govEvents(t, f.pool, f.p.ID) {
		if ev.Event == auth.AuditAgentChangeConfirmed {
			confirmed++
		}
	}
	require.Equal(t, 1, confirmed, "one confirmed event")
}

func TestChange_ConcurrentRequestsLeaveOnePending(t *testing.T) {
	f := newChangeFixture(t)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.s.RequestChange(context.Background(), ChangeRequest{
				PrincipalID: f.p.ID, Patch: Patch{EnvCeiling: envPtr(EnvProd)},
				Actor: "a@example.com", TTL: time.Minute, Audit: Audit{ActorUserID: f.a}})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	var pending, superseded int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT
		count(*) FILTER (WHERE status = 'pending'),
		count(*) FILTER (WHERE status = 'superseded')
		FROM sage.guard_principal_changes WHERE principal_id = $1`, f.p.ID).
		Scan(&pending, &superseded))
	require.Equal(t, 1, pending)
	require.Equal(t, 3, superseded)
}

func TestChange_FailedAuditFailsConfirmAndRequest(t *testing.T) {
	f := newChangeFixture(t)
	prod := Patch{EnvCeiling: envPtr(EnvProd)}
	pc := f.request(t, f.a, prod)
	prefix := fmt.Sprintf("chgfail%d", uniqSeq.Add(1))
	_, err := f.pool.Exec(context.Background(),
		"UPDATE sage.guard_principals SET name = $2 WHERE id = $1", f.p.ID, prefix+"-bot")
	require.NoError(t, err)
	refuseAudit(t, f.pool, prefix)
	_, err = f.confirm(f.b, pc.ID, prod)
	require.Error(t, err)
	require.Contains(t, err.Error(), "audit")
	require.Equal(t, EnvStage, mustGet(t, f.s, f.p.ID).EnvCeiling, "no change without its event")
	status, _ := changeStatus(t, f.pool, pc.ID)
	require.Equal(t, "pending", status, "the request survives a failed confirm")
	_, err = f.s.RequestChange(context.Background(), ChangeRequest{PrincipalID: f.p.ID,
		Patch: prod, Actor: "c@example.com", TTL: time.Minute,
		Audit: Audit{ActorUserID: f.c}})
	require.Error(t, err)
	status, _ = changeStatus(t, f.pool, pc.ID)
	require.Equal(t, "pending", status, "a refused request supersedes nothing")
}

func TestChange_RetiredPrincipalIsFinal(t *testing.T) {
	f := newChangeFixture(t)
	prod := Patch{EnvCeiling: envPtr(EnvProd)}
	pc := f.request(t, f.a, prod)
	_, err := f.s.SetStatus(context.Background(), f.p.ID, StatusRetired,
		Audit{ActorUserID: f.a})
	require.NoError(t, err)
	_, err = f.confirm(f.b, pc.ID, prod)
	require.ErrorIs(t, err, ErrRetired)
	_, err = f.s.RequestChange(context.Background(), ChangeRequest{PrincipalID: f.p.ID,
		Patch: prod, Actor: "a@example.com", TTL: time.Minute, Audit: Audit{ActorUserID: f.a}})
	require.ErrorIs(t, err, ErrRetired)
	_, err = f.s.RequestChange(context.Background(), ChangeRequest{
		PrincipalID: "agp_aaaaaaaaaaaaaaaaaaaa", Patch: prod, Actor: "a@example.com",
		TTL: time.Minute, Audit: Audit{ActorUserID: f.a}})
	require.ErrorIs(t, err, ErrNotFound)
}
