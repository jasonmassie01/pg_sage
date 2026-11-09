package agentguard

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agentgov-05: after a kill the sponsor may not be the second admin. The
// sponsor cannot get around that by handing the sponsorship to someone
// else first: a sponsor change is refused while the freeze is open.
func TestUnfreeze_SponsorCannotReassignThenApprove(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	_, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	a, b := f.newAdmin(t), f.newAdmin(t)
	sponsor := admin{id: *p.SponsorUserID, email: "sponsor@example.com"}
	stranger := createUser(t, f.super, "operator")

	res, err := f.sw.Unfreeze(ctx, unfreezeBy(p, a, "incident closed"))
	require.NoError(t, err)
	require.True(t, res.Pending)

	_, err = f.store.Update(ctx, p.ID, Patch{SponsorUserID: &stranger},
		Audit{ActorUserID: sponsor.id, Reason: "hand-over"})
	require.ErrorIs(t, err, ErrSponsorChangeFrozen)
	require.Equal(t, sponsor.id, *mustGet(t, f.store, p.ID).SponsorUserID)

	_, err = f.sw.Unfreeze(ctx, unfreezeBy(p, sponsor, "ok"))
	require.ErrorIs(t, err, ErrSponsorCannotApprove)
	require.Equal(t, StatusFrozen, mustGet(t, f.store, p.ID).Status,
		"the sponsor alone never lifts a kill")

	done, err := f.sw.Unfreeze(ctx, unfreezeBy(p, b, "agreed"))
	require.NoError(t, err)
	require.True(t, done.Applied)
	moved, err := f.store.Update(ctx, p.ID, Patch{SponsorUserID: &stranger},
		Audit{ActorUserID: sponsor.id})
	require.NoError(t, err, "after the unfreeze the sponsor may change")
	require.Equal(t, stranger, *moved.SponsorUserID)
}
