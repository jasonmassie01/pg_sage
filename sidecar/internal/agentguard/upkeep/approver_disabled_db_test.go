package upkeep

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// apiauth-01: a deactivated admin approves nothing, so the retire grace
// job skips (and reports) a retirement whose approver was deactivated,
// exactly as it does for one whose approver was demoted. Re-enabling the
// approver lets the next run drop the roles.
func TestDropRetired_DeactivatedApproverSkipsUntilReEnabled(t *testing.T) {
	pool := livePool(t)
	admin := createUser(t, pool, "admin")
	approver := createUser(t, pool, "admin")
	key := uniqKey()
	p := newPrincipal(t, pool, admin)
	registerRoles(t, pool, p, key, agentguard.RoleStatusActive, time.Hour)
	retireAgo(t, pool, p, approver, week+time.Hour)
	ctx := context.Background()
	_, err := pool.Exec(ctx, "UPDATE sage.users SET disabled_at = now() WHERE id = $1",
		approver)
	require.NoError(t, err)

	roles := &fakeRoles{}
	rep, err := runner(t, pool, roles, weekly(), target(pool, key)).DropRetired(ctx, Fence{})
	require.NoError(t, err)
	o, ok := outcomeFor(rep.Skipped, p.ID)
	require.True(t, ok, "a deactivated approver's retirement must be skipped: %+v", rep)
	require.Equal(t, ReasonApproverInactive, o.Reason)
	require.Contains(t, o.Detail, "no longer an enabled operator or admin")
	require.Equal(t, 0, len(retiresOf(roles, p.ID)), "no role dropped on a dead approval")

	_, err = pool.Exec(ctx, "UPDATE sage.users SET disabled_at = NULL WHERE id = $1",
		approver)
	require.NoError(t, err)
	_, err = runner(t, pool, roles, weekly(), target(pool, key)).DropRetired(ctx, Fence{})
	require.NoError(t, err)
	require.Equal(t, 1, len(retiresOf(roles, p.ID)), "re-enabled approver's retirement runs")
}
