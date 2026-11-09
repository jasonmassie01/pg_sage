package agentguard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-05: a sponsor is an enabled operator or admin, on create, on
// change and when read; and a sponsor cannot be changed while the
// principal has an open freeze (the unfreeze sign-off reads it).

func TestSponsor_MustBeEnabledOperatorOrAdmin(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	viewer, disabled := createUser(t, pool, "viewer"), createUser(t, pool, "operator")
	_, err := pool.Exec(ctx, "UPDATE sage.users SET disabled_at = now() WHERE id = $1",
		disabled)
	require.NoError(t, err)
	for name, id := range map[string]int{"viewer": viewer, "disabled": disabled} {
		_, err := s.Create(ctx, CreateRequest{Name: uniqName("ineligible"),
			SponsorUserID: &id, Profile: "readonly-analyst", CreatedBy: "a"})
		require.ErrorIs(t, err, ErrSponsorNotEligible, "create with a %s sponsor", name)
	}
	for _, role := range []string{"operator", "admin"} {
		id := createUser(t, pool, role)
		p := newPrincipal(t, s, &id)
		require.True(t, p.SponsorActive, "an %s sponsors", role)
	}
	ok := createUser(t, pool, "operator")
	p := newPrincipal(t, s, &ok)
	for name, id := range map[string]int{"viewer": viewer, "disabled": disabled} {
		_, err := s.Update(ctx, p.ID, Patch{SponsorUserID: &id}, Audit{})
		require.ErrorIs(t, err, ErrSponsorNotEligible, "patch to a %s sponsor", name)
	}
	missing := 2147483000
	_, err = s.Update(ctx, p.ID, Patch{SponsorUserID: &missing}, Audit{})
	require.ErrorIs(t, err, ErrSponsorNotFound)
	require.Equal(t, ok, *mustGet(t, s, p.ID).SponsorUserID, "refusals change nothing")
	require.Len(t, govEvents(t, pool, p.ID), 1, "refusals write no event")
}

func TestSponsor_DemotedOrDisabledSponsorIsInactive(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	sponsor := createUser(t, pool, "operator")
	p := newPrincipal(t, s, &sponsor)
	require.True(t, ToolAccess(p, ToolAgent).Allowed, "an operator sponsor is active")

	_, err := pool.Exec(ctx, "UPDATE sage.users SET role = 'viewer' WHERE id = $1", sponsor)
	require.NoError(t, err)
	demoted := mustGet(t, s, p.ID)
	require.Equal(t, sponsor, *demoted.SponsorUserID, "the sponsor id stays on record")
	require.False(t, demoted.SponsorActive, "a viewer sponsors nothing")
	require.False(t, demoted.Sponsored())
	require.Equal(t, ReasonUnsponsored, ToolAccess(demoted, ToolAgent).Reason)
	require.True(t, ToolAccess(demoted, ToolPropose).Allowed, "proposals need no sponsor")

	_, err = pool.Exec(ctx, "UPDATE sage.users SET role = 'admin' WHERE id = $1", sponsor)
	require.NoError(t, err)
	require.True(t, mustGet(t, s, p.ID).SponsorActive, "promoted again: active")
	_, err = pool.Exec(ctx, "UPDATE sage.users SET disabled_at = now() WHERE id = $1",
		sponsor)
	require.NoError(t, err)
	require.False(t, mustGet(t, s, p.ID).SponsorActive, "a disabled sponsor is inactive")
	page, err := s.List(ctx, ListOptions{Limit: 200, Cursor: ""})
	require.NoError(t, err)
	for _, q := range page.Items {
		if q.ID == p.ID {
			require.False(t, q.SponsorActive, "the list reads the same rule")
		}
	}
}

// freezePrincipal freezes the principal the way a kill does (kill_id
// NULL: an operator freeze row).
func freezePrincipal(ctx context.Context, s *Store, id string) error {
	return pgx.BeginFunc(ctx, s.Pool(), func(tx pgx.Tx) error {
		ids, err := lockPrincipals(ctx, tx, freezeMark{ids: []string{id}}, 5000)
		if err != nil {
			return err
		}
		return writeFreeze(ctx, tx, freezeMark{ids: ids, reason: "test", actor: "t"}, ids)
	})
}

// freezeNow freezes the principal and returns a func that lifts it.
func freezeNow(t *testing.T, s *Store, id string) func() {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, freezePrincipal(ctx, s, id))
	return func() {
		_, err := s.Pool().Exec(ctx, `UPDATE sage.guard_freezes SET cleared_at = now(),
			cleared_by = 'test' WHERE scope = 'principal' AND target = $1
			AND cleared_at IS NULL`, id)
		require.NoError(t, err)
		_, err = s.Pool().Exec(ctx, `UPDATE sage.guard_principals SET status = 'active',
			frozen_reason = '' WHERE id = $1`, id)
		require.NoError(t, err)
	}
}

func TestSponsor_ChangeRefusedWhileFrozen(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	first, second := createUser(t, pool, "admin"), createUser(t, pool, "operator")
	p := newPrincipal(t, s, &first)
	lift := freezeNow(t, s, p.ID)

	_, err := s.Update(ctx, p.ID, Patch{SponsorUserID: &second}, Audit{ActorUserID: first})
	require.ErrorIs(t, err, ErrSponsorChangeFrozen)
	got := mustGet(t, s, p.ID)
	require.Equal(t, first, *got.SponsorUserID)
	require.Equal(t, StatusFrozen, got.Status)
	same, dev := first, EnvDev
	_, err = s.Update(ctx, p.ID, Patch{SponsorUserID: &same, EnvCeiling: &dev}, Audit{})
	require.NoError(t, err, "naming the same sponsor and narrowing are no sponsor change")
	require.Len(t, govEvents(t, pool, p.ID), 2, "create and the narrowing")

	lift()
	moved, err := s.Update(ctx, p.ID, Patch{SponsorUserID: &second}, Audit{})
	require.NoError(t, err, "after the freeze is lifted the sponsor may change")
	require.Equal(t, second, *moved.SponsorUserID)
}

// TestSponsor_ChangeRacesFreeze: a sponsor change and a freeze in
// parallel end either frozen with the old sponsor (change refused, no
// event) or frozen with the new sponsor and its event.
func TestSponsor_ChangeRacesFreeze(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	first, second := createUser(t, pool, "admin"), createUser(t, pool, "operator")
	outcomes := map[string]int{}
	for i := 0; i < 12; i++ {
		p := newPrincipal(t, s, &first)
		var wg sync.WaitGroup
		var updErr, freezeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i%4) * time.Millisecond)
			_, updErr = s.Update(ctx, p.ID, Patch{SponsorUserID: &second}, Audit{})
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration((i+2)%4) * time.Millisecond)
			freezeErr = freezePrincipal(ctx, s, p.ID)
		}()
		wg.Wait()
		require.NoError(t, freezeErr, "run %d: the freeze", i)
		got := mustGet(t, s, p.ID)
		require.Equal(t, StatusFrozen, got.Status)
		updated := 0
		for _, e := range govEvents(t, pool, p.ID) {
			if e.Event == "agent_updated" {
				updated++
			}
		}
		switch {
		case updErr == nil:
			require.Equal(t, second, *got.SponsorUserID, "run %d", i)
			require.Equal(t, 1, updated, "run %d: the change has its event", i)
			outcomes["changed-then-frozen"]++
		case errors.Is(updErr, ErrSponsorChangeFrozen):
			require.Equal(t, first, *got.SponsorUserID, "run %d", i)
			require.Equal(t, 0, updated, "run %d: a refused change has no event", i)
			outcomes["frozen-then-refused"]++
		default:
			t.Fatalf("run %d: update error %v", i, updErr)
		}
	}
	t.Logf("race outcomes: %v", outcomes)
}

// TestSponsor_ChangeWaitsForAFreezeInFlight: a sponsor change that starts
// while a kill holds the principal's row (freeze written, not committed)
// waits for it and then sees the freeze: it is refused, not applied on a
// snapshot that predates the kill.
func TestSponsor_ChangeWaitsForAFreezeInFlight(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	first, second := createUser(t, pool, "admin"), createUser(t, pool, "operator")
	p := newPrincipal(t, s, &first)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	ids, err := lockPrincipals(ctx, tx, freezeMark{ids: []string{p.ID}}, 5000)
	require.NoError(t, err)
	require.NoError(t, writeFreeze(ctx, tx, freezeMark{ids: ids, reason: "kill",
		actor: "t"}, ids))
	done := make(chan error, 1)
	go func() {
		_, err := s.Update(ctx, p.ID, Patch{SponsorUserID: &second}, Audit{})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the sponsor change did not wait for the kill in flight: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	require.NoError(t, tx.Commit(ctx))
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrSponsorChangeFrozen)
	case <-time.After(15 * time.Second):
		t.Fatal("the sponsor change did not finish after the kill committed")
	}
	got := mustGet(t, s, p.ID)
	require.Equal(t, first, *got.SponsorUserID, "the kill's sponsor stands")
	require.Equal(t, StatusFrozen, got.Status)
	require.Len(t, govEvents(t, pool, p.ID), 1, "only the create is recorded")
}
