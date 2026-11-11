package agentguard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-03: OAuth identity bindings on real PostgreSQL. A binding is
// one (issuer, subject) row per identity; bind and unbind each write one
// chained auth_audit event in their own transaction, a retired or frozen
// principal gets no new binding, and a duplicate identity is
// ErrIdentityBound whichever principal holds it.

const testIssuer = "https://idp.example.com"

func bindReq(subject string, actor int) BindRequest {
	return BindRequest{Issuer: testIssuer, Subject: subject, CreatedBy: "admin@example.com",
		Audit: Audit{ActorUserID: actor, SourceIP: "203.0.113.7"}}
}

func bindingCount(t *testing.T, s *Store, issuer, subject string) int {
	t.Helper()
	var n int
	require.NoError(t, s.Pool().QueryRow(context.Background(), `SELECT count(*)
		FROM sage.guard_identity_bindings WHERE issuer = $1 AND subject = $2`,
		issuer, subject).Scan(&n))
	return n
}

func TestIdentities_BindListUnbind(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	actor, sponsor := createUser(t, pool, "admin"), createUser(t, pool, "operator")
	p := newPrincipal(t, s, &sponsor)
	subject := uniqName("client")

	empty, err := s.ListIdentities(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, empty.Items, 0)
	require.False(t, empty.Truncated)

	b, err := s.BindIdentity(ctx, p.ID, bindReq(subject, actor))
	require.NoError(t, err)
	require.Equal(t, testIssuer, b.Issuer)
	require.Equal(t, subject, b.Subject)
	require.Equal(t, p.ID, b.PrincipalID)
	require.Equal(t, "admin@example.com", b.CreatedBy)
	require.False(t, b.CreatedAt.IsZero(), "created_at comes from the row")

	list, err := s.ListIdentities(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	require.Equal(t, b, list.Items[0])

	ev := govEvents(t, pool, p.ID)
	require.Len(t, ev, 2, "agent_created then agent_identity_bound")
	require.Equal(t, auth.AuditAgentIdentityBound, ev[1].Event)
	require.Equal(t, actor, ev[1].Actor)
	require.Equal(t, sponsor, ev[1].Target, "target_user_id is the sponsor")
	require.Equal(t, "203.0.113.7", ev[1].IP)
	require.Equal(t, testIssuer, ev[1].Detail["issuer"])
	require.Equal(t, subject, ev[1].Detail["subject"])
	requireChained(t, pool, ev[1].ID)
	checkUnbind(t, s, b, actor)
}

// checkUnbind removes binding b, checks its event, and that a second
// unbind is ErrIdentityNotBound and writes nothing.
func checkUnbind(t *testing.T, s *Store, b IdentityBinding, actor int) {
	t.Helper()
	ctx, pool, subject := context.Background(), s.Pool(), b.Subject
	gone, err := s.UnbindIdentity(ctx, b.PrincipalID, testIssuer, subject,
		Audit{ActorUserID: actor, SourceIP: "203.0.113.8"})
	require.NoError(t, err)
	require.Equal(t, b, gone, "unbind returns the removed binding")
	require.Equal(t, 0, bindingCount(t, s, testIssuer, subject))
	ev := govEvents(t, pool, b.PrincipalID)
	require.Len(t, ev, 3)
	require.Equal(t, auth.AuditAgentIdentityUnbound, ev[2].Event)
	require.Equal(t, subject, ev[2].Detail["subject"])
	require.Equal(t, "203.0.113.8", ev[2].IP)
	requireChained(t, pool, ev[2].ID)

	_, err = s.UnbindIdentity(ctx, b.PrincipalID, testIssuer, subject,
		Audit{ActorUserID: actor})
	require.True(t, errors.Is(err, ErrIdentityNotBound), "second unbind: %v", err)
	require.Len(t, govEvents(t, pool, b.PrincipalID), 3, "a refused unbind writes nothing")
}

func TestIdentities_DuplicateIsErrIdentityBound(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	actor := createUser(t, pool, "admin")
	first, second := newPrincipal(t, s, &actor), newPrincipal(t, s, &actor)
	subject := uniqName("dup")
	_, err := s.BindIdentity(ctx, first.ID, bindReq(subject, actor))
	require.NoError(t, err)
	_, err = s.BindIdentity(ctx, first.ID, bindReq(subject, actor))
	require.True(t, errors.Is(err, ErrIdentityBound), "same principal: %v", err)
	_, err = s.BindIdentity(ctx, second.ID, bindReq(subject, actor))
	require.True(t, errors.Is(err, ErrIdentityBound), "another principal: %v", err)
	require.Len(t, govEvents(t, pool, second.ID), 1, "a refused bind writes no event")

	// The same subject at another issuer is another identity.
	other := bindReq(subject, actor)
	other.Issuer = "https://other-idp.example.com"
	_, err = s.BindIdentity(ctx, second.ID, other)
	require.NoError(t, err)
}

// An unbind names the principal: an identity bound to one principal cannot
// be removed through another's id, and the refusal writes nothing.
func TestIdentities_UnbindThroughAnotherPrincipalIsNotBound(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	actor := createUser(t, pool, "admin")
	holder, other := newPrincipal(t, s, &actor), newPrincipal(t, s, &actor)
	subject := uniqName("held")
	_, err := s.BindIdentity(ctx, holder.ID, bindReq(subject, actor))
	require.NoError(t, err)
	_, err = s.UnbindIdentity(ctx, other.ID, testIssuer, subject, Audit{ActorUserID: actor})
	require.True(t, errors.Is(err, ErrIdentityNotBound), "through another principal: %v", err)
	require.Equal(t, 1, bindingCount(t, s, testIssuer, subject), "the binding stays")
	require.Len(t, govEvents(t, pool, other.ID), 1, "a refused unbind writes no event")
	list, err := s.ListIdentities(ctx, holder.ID)
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	require.Equal(t, holder.ID, list.Items[0].PrincipalID)
}

// Two admins bind one identity to two principals at once: exactly one
// wins, the other is ErrIdentityBound, and the row names the winner.
func TestIdentities_ConcurrentBindOneWins(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	actor := createUser(t, pool, "admin")
	subject := uniqName("race")
	const n = 6
	ids := make([]string, n)
	for i := range ids {
		ids[i] = newPrincipal(t, s, &actor).ID
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.BindIdentity(context.Background(), ids[i], bindReq(subject, actor))
		}(i)
	}
	wg.Wait()
	winner := ""
	for i, err := range errs {
		if err == nil {
			require.Equal(t, "", winner, "two binds succeeded")
			winner = ids[i]
			continue
		}
		require.True(t, errors.Is(err, ErrIdentityBound), "bind %d: %v", i, err)
	}
	require.NotEqual(t, "", winner, "no bind succeeded: %v", errs)
	var bound string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT principal_id
		FROM sage.guard_identity_bindings WHERE issuer = $1 AND subject = $2`,
		testIssuer, subject).Scan(&bound))
	require.Equal(t, winner, bound)
}

func TestIdentities_RetiredAndFrozenGetNoBinding(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	actor := createUser(t, pool, "admin")
	frozen, retired := newPrincipal(t, s, &actor), newPrincipal(t, s, &actor)
	keep := uniqName("kept")
	_, err := s.BindIdentity(ctx, retired.ID, bindReq(keep, actor))
	require.NoError(t, err)

	_, err = s.SetStatus(ctx, frozen.ID, StatusFrozen, Audit{Reason: "odd queries"})
	require.NoError(t, err)
	subject := uniqName("frozen")
	_, err = s.BindIdentity(ctx, frozen.ID, bindReq(subject, actor))
	d, ok := IsDenied(err)
	require.True(t, ok, "frozen: %v", err)
	require.Equal(t, ReasonFrozen, d.Reason)
	require.Equal(t, 0, bindingCount(t, s, testIssuer, subject))

	_, err = s.SetStatus(ctx, retired.ID, StatusRetired, Audit{Reason: "replaced"})
	require.NoError(t, err)
	_, err = s.BindIdentity(ctx, retired.ID, bindReq(uniqName("late"), actor))
	require.True(t, errors.Is(err, ErrRetired), "retired: %v", err)

	// Unbinding narrows, so a retired principal's binding can still go.
	_, err = s.UnbindIdentity(ctx, retired.ID, testIssuer, keep, Audit{ActorUserID: actor})
	require.NoError(t, err)
	require.Equal(t, 0, bindingCount(t, s, testIssuer, keep))
}

func TestIdentities_UnknownPrincipal(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	const missing = "agp_zzzzzzzzzzzzzzzzzzzz"
	_, err := s.BindIdentity(ctx, missing, bindReq("x", 0))
	require.True(t, errors.Is(err, ErrNotFound), "bind: %v", err)
	_, err = s.ListIdentities(ctx, missing)
	require.True(t, errors.Is(err, ErrNotFound), "list: %v", err)
	_, err = s.UnbindIdentity(ctx, missing, testIssuer, "x", Audit{})
	require.True(t, errors.Is(err, ErrNotFound), "unbind: %v", err)
	_, err = s.BindIdentity(ctx, "not-an-id", bindReq("x", 0))
	require.True(t, errors.Is(err, ErrNotFound), "malformed id: %v", err)
}

// A failed audit insert fails the bind and the unbind: no row without its
// event, and no removal without its event.
func TestIdentities_RefusedAuditChangesNothing(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	actor := createUser(t, pool, "admin")
	p := newPrincipal(t, s, &actor)
	kept := uniqName("kept")
	_, err := s.BindIdentity(ctx, p.ID, bindReq(kept, actor))
	require.NoError(t, err)
	fn := strings.ReplaceAll(uniqName("refuse_identity_audit"), "-", "_")
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION sage.%s() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.detail->>'principal_id' = '%s' THEN RAISE EXCEPTION 'audit refused'; END IF;
		RETURN NEW; END $$`, fn, p.ID))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON sage.auth_audit
		FOR EACH ROW EXECUTE FUNCTION sage.%s()`, fn, fn))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS "+fn+
			" ON sage.auth_audit")
		_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS sage."+fn+"()")
	})
	subject := uniqName("unaudited")
	_, err = s.BindIdentity(ctx, p.ID, bindReq(subject, actor))
	require.Error(t, err)
	require.Contains(t, err.Error(), "audit refused")
	require.Equal(t, 0, bindingCount(t, s, testIssuer, subject), "no row without its event")
	_, err = s.UnbindIdentity(ctx, p.ID, testIssuer, kept, Audit{ActorUserID: actor})
	require.Error(t, err)
	require.Equal(t, 1, bindingCount(t, s, testIssuer, kept), "no removal without its event")
}

// Retiring a principal leaves its bindings in place but the MCP resolver
// (which skips retired principals) no longer maps them; the row is kept
// for the audit trail and dropped with the principal.
func TestIdentities_BindingSurvivesRetirementRow(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	ctx := context.Background()
	actor := createUser(t, pool, "admin")
	p := newPrincipal(t, s, &actor)
	subject := uniqName("retiring")
	_, err := s.BindIdentity(ctx, p.ID, bindReq(subject, actor))
	require.NoError(t, err)
	_, err = s.SetStatus(ctx, p.ID, StatusRetired, Audit{Reason: "done"})
	require.NoError(t, err)
	list, err := s.ListIdentities(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, list.Items, 1, "a retired principal's bindings are still listed")
}
