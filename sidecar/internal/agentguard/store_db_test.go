package agentguard

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestStore_CreateAndGet(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	sponsor := createUser(t, pool, "operator")
	name := uniqName("billing")
	before := time.Now().Add(-time.Minute)
	p, err := s.Create(ctx, CreateRequest{Name: name, SponsorUserID: &sponsor,
		Tenant: "acme", Profile: "app-writer", EnvCeiling: EnvStage,
		CreatedBy: "admin@example.com"})
	require.NoError(t, err)
	require.True(t, ValidID(p.ID), "id %q", p.ID)
	require.Equal(t, name, p.Name)
	require.Equal(t, sponsor, *p.SponsorUserID)
	require.True(t, p.SponsorActive)
	require.True(t, p.Sponsored())
	require.Equal(t, "acme", p.Tenant)
	require.Equal(t, "app-writer", p.Profile)
	require.Equal(t, EnvStage, p.EnvCeiling)
	require.Equal(t, StatusActive, p.Status)
	require.False(t, p.Tainted)
	require.Equal(t, "admin@example.com", p.CreatedBy)
	require.True(t, p.CreatedAt.After(before))
	byName, err := s.GetByName(ctx, name)
	require.NoError(t, err)
	require.Equal(t, p.ID, byName.ID)
	got, err := s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, p, got)
}

func TestStore_CreateDefaultsAndUnsponsored(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	p, err := s.Create(context.Background(), CreateRequest{Name: uniqName("legacy"),
		Profile: "legacy", CreatedBy: "migration"})
	require.NoError(t, err)
	require.Equal(t, EnvDev, p.EnvCeiling, "the table default")
	require.Nil(t, p.SponsorUserID)
	require.False(t, p.Sponsored())
	require.Equal(t, "", p.Tenant)
}

func TestStore_CreateErrors(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	p := newPrincipal(t, s, nil)
	_, err := s.Create(ctx, CreateRequest{Name: p.Name, Profile: "legacy", CreatedBy: "a"})
	require.ErrorIs(t, err, ErrDuplicateName)
	missing := 2147483000
	_, err = s.Create(ctx, CreateRequest{Name: uniqName("orphan"), SponsorUserID: &missing,
		Profile: "legacy", CreatedBy: "a"})
	require.ErrorIs(t, err, ErrSponsorNotFound)
	_, err = s.Get(ctx, "agp_aaaaaaaaaaaaaaaaaaaa")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.Get(ctx, "not-an-id")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.GetByName(ctx, "no-such-bot-anywhere")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStore_ConcurrentCreateSameName(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	name := uniqName("race")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.Create(context.Background(), CreateRequest{Name: name,
				Profile: "legacy", CreatedBy: "a"})
		}(i)
	}
	wg.Wait()
	ok, dup := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrDuplicateName):
			dup++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, ok)
	require.Equal(t, 7, dup)
}

func TestStore_ListPages(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	created := map[string]bool{}
	for i := 0; i < 5; i++ {
		created[newPrincipal(t, s, nil).Name] = true
	}
	var all []string
	cursor := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 1000, "pagination does not end")
		page, err := s.List(ctx, ListOptions{Limit: 2, Cursor: cursor})
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Items), 2)
		for _, p := range page.Items {
			all = append(all, p.Name)
		}
		if page.NextCursor == "" {
			break
		}
		require.Equal(t, page.Items[len(page.Items)-1].Name, page.NextCursor)
		cursor = page.NextCursor
	}
	require.True(t, sort.StringsAreSorted(all), "pages are in name order")
	seen := map[string]bool{}
	for _, n := range all {
		require.False(t, seen[n], "duplicate %s across pages", n)
		seen[n] = true
	}
	for n := range created {
		require.True(t, seen[n], "missing %s", n)
	}
}

func TestStore_ListByStatus(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	p := newPrincipal(t, s, nil)
	_, err := s.SetStatus(ctx, p.ID, StatusFrozen, "test")
	require.NoError(t, err)
	page, err := s.List(ctx, ListOptions{Limit: 200, Status: StatusFrozen})
	require.NoError(t, err)
	found := false
	for _, q := range page.Items {
		require.Equal(t, StatusFrozen, q.Status)
		found = found || q.ID == p.ID
	}
	require.True(t, found)
}

func TestStore_UpdateAndSponsorDeparture(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	first, second := createUser(t, pool, "admin"), createUser(t, pool, "viewer")
	p := newPrincipal(t, s, &first)
	dev, profile := EnvDev, "coding-agent"
	got, err := s.Update(ctx, p.ID, Patch{SponsorUserID: &second, EnvCeiling: &dev,
		Profile: &profile})
	require.NoError(t, err)
	require.Equal(t, second, *got.SponsorUserID)
	require.Equal(t, EnvDev, got.EnvCeiling)
	require.Equal(t, "coding-agent", got.Profile)
	require.True(t, got.UpdatedAt.After(p.UpdatedAt) || got.UpdatedAt.Equal(p.UpdatedAt))
	unchanged, err := s.Update(ctx, p.ID, Patch{})
	require.NoError(t, err)
	require.Equal(t, "coding-agent", unchanged.Profile)
	missing := 2147483000
	_, err = s.Update(ctx, p.ID, Patch{SponsorUserID: &missing})
	require.ErrorIs(t, err, ErrSponsorNotFound)
	// The sponsor leaves: the principal becomes unsponsored (L0, ID-2).
	_, err = pool.Exec(ctx, "DELETE FROM sage.users WHERE id = $1", second)
	require.NoError(t, err)
	orphan, err := s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Nil(t, orphan.SponsorUserID)
	require.False(t, orphan.Sponsored())
	require.Equal(t, ReasonUnsponsored, ToolAccess(orphan, ToolAgent).Reason)
	_, err = s.Update(ctx, "agp_aaaaaaaaaaaaaaaaaaaa", Patch{})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStore_StatusTransitions(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	p := newPrincipal(t, s, nil)
	frozen, err := s.SetStatus(ctx, p.ID, StatusFrozen, "canary read")
	require.NoError(t, err)
	require.Equal(t, StatusFrozen, frozen.Status)
	require.Equal(t, "canary read", frozen.FrozenReason)
	active, err := s.SetStatus(ctx, p.ID, StatusActive, "ignored")
	require.NoError(t, err)
	require.Equal(t, StatusActive, active.Status)
	require.Equal(t, "", active.FrozenReason, "the reason is cleared on unfreeze")
	retired, err := s.SetStatus(ctx, p.ID, StatusRetired, "")
	require.NoError(t, err)
	require.Equal(t, StatusRetired, retired.Status)
	for _, to := range []Status{StatusActive, StatusFrozen, StatusRetired} {
		_, err = s.SetStatus(ctx, p.ID, to, "")
		require.ErrorIs(t, err, ErrRetired, "retired is final (%s)", to)
	}
	dev := EnvDev
	_, err = s.Update(ctx, p.ID, Patch{EnvCeiling: &dev})
	require.ErrorIs(t, err, ErrRetired)
	_, err = s.SetStatus(ctx, "agp_aaaaaaaaaaaaaaaaaaaa", StatusFrozen, "")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestStore_TaintLifecycle(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	p := newPrincipal(t, s, nil)
	require.NoError(t, s.Taint(ctx, p.ID, "", "agent_query on support.tickets"))
	require.NoError(t, s.Taint(ctx, p.ID, "", "agent_query on support.tickets"),
		"re-tainting is a no-op")
	got, err := s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, got.Tainted)
	n, err := s.ClearTaint(ctx, p.ID, "ops@example.com")
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	got, err = s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.False(t, got.Tainted)
	n, err = s.ClearTaint(ctx, p.ID, "ops@example.com")
	require.NoError(t, err)
	require.Equal(t, int64(0), n, "nothing left to clear")
	// A new read re-opens the cleared row; it has no time expiry.
	require.NoError(t, s.Taint(ctx, p.ID, "", "agent_query on support.tickets"))
	got, err = s.Get(ctx, p.ID)
	require.NoError(t, err)
	require.True(t, got.Tainted)
	var clearedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT cleared_at FROM sage.guard_taint
		WHERE principal_id = $1`, p.ID).Scan(&clearedAt))
	require.Nil(t, clearedAt)
	require.ErrorIs(t, s.Taint(ctx, "agp_aaaaaaaaaaaaaaaaaaaa", "", "x"), ErrNotFound)
	_, err = s.ClearTaint(ctx, p.ID, "")
	require.ErrorIs(t, err, ErrInvalid)
}

func TestStore_TaintPerTask(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	p := newPrincipal(t, s, nil)
	require.NoError(t, s.Taint(ctx, p.ID, "task-1", "read a"))
	require.NoError(t, s.Taint(ctx, p.ID, "task-2", "read a"))
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM sage.guard_taint
		WHERE principal_id = $1 AND cleared_at IS NULL`, p.ID).Scan(&n))
	require.Equal(t, 2, n)
	cleared, err := s.ClearTaint(ctx, p.ID, "context_reset")
	require.NoError(t, err)
	require.Equal(t, int64(2), cleared)
}
