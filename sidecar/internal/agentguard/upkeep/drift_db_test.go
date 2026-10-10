package upkeep

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// §6.6's daily drift reconciler on a live cluster (PG16+): each agent
// role's attributes, memberships and privileges against what governance
// recorded. Widening drift is a critical finding; narrowing corrections
// that governance owns run as guard_revoke (narrowing: at any trust
// level); anything else is reported with the statement a person runs.

// driftCluster is a live cluster with an ensured principal and the PUBLIC
// baseline recorded.
func driftCluster(t *testing.T) (*liveCluster, agentguard.Principal) {
	t.Helper()
	c := newLiveCluster(t)
	op := createUser(t, c.super, "operator")
	p := newPrincipal(t, c.super, op)
	c.ensure(t, p, op)
	_, err := agentguard.RecordPublicBaseline(context.Background(), c.super)
	require.NoError(t, err)
	return c, p
}

func (c *liveCluster) driftRunner(t *testing.T) *Runner {
	r, err := New(c.super, c.manager, targetsOf(c.target()), weekly())
	require.NoError(t, err)
	return r
}

func driftOf(rep DriftReport, role string) (RoleDrift, bool) {
	for _, d := range rep.Drift {
		if d.Role == role {
			return d, true
		}
	}
	return RoleDrift{}, false
}

// ownTable creates a table owned by pg_sage's role (so pg_sage is its
// grantor) and returns its name.
func (c *liveCluster) ownTable(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("public.drift_%d_%d", time.Now().UnixNano()%1e9, seq.Add(1))
	ctx := context.Background()
	_, err := c.super.Exec(ctx, "GRANT CREATE ON SCHEMA public TO "+c.name)
	require.NoError(t, err)
	_, err = c.admin.Exec(ctx, "CREATE TABLE "+name+" (id int, secret text)")
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := c.super.Exec(context.Background(), "DROP TABLE IF EXISTS "+name); err != nil {
			t.Errorf("cleanup: drop %s: %v", name, err)
		}
	})
	return name
}

func (c *liveCluster) can(t *testing.T, role, table, priv string) bool {
	var ok bool
	require.NoError(t, c.super.QueryRow(context.Background(),
		"SELECT has_table_privilege($1, $2, $3)", role, table, priv).Scan(&ok))
	return ok
}

func TestDrift_CleanRolesHaveNoDriftAndNoFinding(t *testing.T) {
	c, p := driftCluster(t)
	rep, err := c.driftRunner(t).ReconcileDrift(context.Background(), Fence{})
	require.NoError(t, err)
	for _, role := range []string{p.BrokerRole(), p.LoginRole()} {
		_, drifted := driftOf(rep, role)
		require.False(t, drifted, "%s: %+v", role, rep)
		require.Equal(t, 0, findingCount(t, c.super, DriftFindingCategory, role, "open"))
	}
	require.True(t, rep.Roles >= 2, "both roles were checked: %+v", rep)
}

func TestDrift_OwnUnregisteredGrantIsRevokedAndRaisedCritical(t *testing.T) {
	c, p := driftCluster(t)
	table := c.ownTable(t)
	ctx := context.Background()
	_, err := c.admin.Exec(ctx, "GRANT SELECT ON "+table+" TO "+p.BrokerRole())
	require.NoError(t, err)
	require.True(t, c.can(t, p.BrokerRole(), table, "SELECT"))
	c.trust = "observation" // a narrowing correction runs at any trust level

	rep, err := c.driftRunner(t).ReconcileDrift(ctx, Fence{})
	require.NoError(t, err)
	d, ok := driftOf(rep, p.BrokerRole())
	require.True(t, ok, "%+v", rep)
	require.Contains(t, strings.Join(d.Widening, ";"), table)
	require.Contains(t, strings.Join(d.Corrected, ";"), "REVOKE SELECT ON TABLE")
	require.True(t, len(d.ActionIDs) == 1, "one guard_revoke action: %+v", d)
	require.False(t, c.can(t, p.BrokerRole(), table, "SELECT"), "the excess is revoked")
	sev, _, _, open := findingOf(t, c.super, DriftFindingCategory, p.BrokerRole())
	require.True(t, open, "widening drift is a finding even when corrected")
	require.Equal(t, "critical", sev)
	var actionType string
	var raw []byte
	require.NoError(t, c.super.QueryRow(ctx, `SELECT action_type, after_state
		FROM sage.action_log WHERE id = $1`, d.ActionIDs[0]).Scan(&actionType, &raw))
	require.Equal(t, executor.ActionTypeGuardRevoke, actionType)
	var after map[string]any
	require.NoError(t, json.Unmarshal(raw, &after))
	require.Equal(t, JobDriftCorrection, after["scheduled"])

	// The next pass finds nothing and resolves the finding.
	rep, err = c.driftRunner(t).ReconcileDrift(ctx, Fence{})
	require.NoError(t, err)
	_, drifted := driftOf(rep, p.BrokerRole())
	require.False(t, drifted, "%+v", rep)
	require.Equal(t, 0, findingCount(t, c.super, DriftFindingCategory, p.BrokerRole(),
		"open"))
}

func TestDrift_ForeignGrantIsReportedNeverForced(t *testing.T) {
	c, p := driftCluster(t)
	table := c.ownTable(t)
	ctx := context.Background()
	// The superuser grants it: another grantor, which pg_sage cannot revoke.
	_, err := c.super.Exec(ctx, "GRANT UPDATE ON "+table+" TO "+p.BrokerRole())
	require.NoError(t, err)
	rep, err := c.driftRunner(t).ReconcileDrift(ctx, Fence{})
	require.NoError(t, err)
	d, ok := driftOf(rep, p.BrokerRole())
	require.True(t, ok, "%+v", rep)
	require.Equal(t, 0, len(d.Corrected), "never forced: %+v", d)
	require.Contains(t, d.Fix, "REVOKE")
	require.Contains(t, d.Fix, p.BrokerRole())
	require.True(t, c.can(t, p.BrokerRole(), table, "UPDATE"), "left for its grantor")
	sev, _, sql, open := findingOf(t, c.super, DriftFindingCategory, p.BrokerRole())
	require.True(t, open)
	require.Equal(t, "critical", sev)
	require.Contains(t, sql, "REVOKE")
	_, err = c.super.Exec(ctx, "REVOKE UPDATE ON "+table+" FROM "+p.BrokerRole())
	require.NoError(t, err)
}

func TestDrift_MembershipIsWideningAndReported(t *testing.T) {
	c, p := driftCluster(t)
	ctx := context.Background()
	group := fmt.Sprintf("drift_group_%d", seq.Add(1))
	_, err := c.super.Exec(ctx, "CREATE ROLE "+group+" NOLOGIN")
	require.NoError(t, err)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = c.super.Exec(bg, "REVOKE "+group+" FROM "+p.BrokerRole())
		if _, err := c.super.Exec(bg, "DROP ROLE "+group); err != nil {
			t.Errorf("cleanup: drop %s: %v", group, err)
		}
	})
	_, err = c.super.Exec(ctx, "GRANT "+group+" TO "+p.BrokerRole())
	require.NoError(t, err)
	rep, err := c.driftRunner(t).ReconcileDrift(ctx, Fence{})
	require.NoError(t, err)
	d, ok := driftOf(rep, p.BrokerRole())
	require.True(t, ok, "%+v", rep)
	require.Contains(t, strings.Join(d.Widening, ";"), group)
	require.Contains(t, d.Fix, "REVOKE "+group+" FROM "+p.BrokerRole())
}

func TestDrift_KilledRoleThatCanLogInIsDisabledAgain(t *testing.T) {
	c, p := driftCluster(t)
	ctx := context.Background()
	require.NoError(t, agentguard.NewStore(c.super).SetClusterRoleStatus(ctx, p.ID, c.key,
		agentguard.RoleStatusKilled, nil))
	c.trust = "observation"
	rep, err := c.driftRunner(t).ReconcileDrift(ctx, Fence{})
	require.NoError(t, err)
	d, ok := driftOf(rep, p.BrokerRole())
	require.True(t, ok, "%+v", rep)
	require.Contains(t, strings.Join(d.Widening, ";"), "log in")
	require.Contains(t, strings.Join(d.Corrected, ";"), "NOLOGIN")
	st, err := agentguard.ReadRoleState(ctx, c.super, p.BrokerRole())
	require.NoError(t, err)
	require.False(t, st.CanLogin)
	require.Equal(t, 0, st.ConnLimit)
}

func TestDrift_WiderConnectionLimitIsNarrowedBack(t *testing.T) {
	c, p := driftCluster(t)
	ctx := context.Background()
	_, err := c.super.Exec(ctx, "ALTER ROLE "+p.BrokerRole()+" CONNECTION LIMIT 50")
	require.NoError(t, err)
	rep, err := c.driftRunner(t).ReconcileDrift(ctx, Fence{})
	require.NoError(t, err)
	d, ok := driftOf(rep, p.BrokerRole())
	require.True(t, ok, "%+v", rep)
	require.Contains(t, strings.Join(d.Widening, ";"), "connection limit")
	st, err := agentguard.ReadRoleState(ctx, c.super, p.BrokerRole())
	require.NoError(t, err)
	require.Equal(t, agentguard.DefaultRoleConfig().BrokerConnectionLimit, st.ConnLimit)
}

func TestDrift_MissingRegistryPrivilegeIsNarrowingWarning(t *testing.T) {
	c, p := driftCluster(t)
	table := c.ownTable(t)
	ctx := context.Background()
	var oid uint32
	require.NoError(t, c.super.QueryRow(ctx, "SELECT $1::regclass::oid", table).Scan(&oid))
	_, err := c.super.Exec(ctx, `INSERT INTO sage.guard_grants (database_id, principal_id,
		lane, capability, object_oid, object_name, columns, privileges, grantor, expires_at,
		grant_action_id) VALUES (gen_random_uuid(), $1, 'broker', 'read', $2, $3,
		'{id}', '{SELECT}', $4, now() + interval '1 hour', 1)`, p.ID, oid, table, c.name)
	require.NoError(t, err)
	rep, err := c.driftRunner(t).ReconcileDrift(ctx, Fence{})
	require.NoError(t, err)
	d, ok := driftOf(rep, p.BrokerRole())
	require.True(t, ok, "%+v", rep)
	require.Equal(t, 0, len(d.Widening), "%+v", d)
	require.Contains(t, strings.Join(d.Narrowing, ";"), "SELECT")
	require.Equal(t, 0, len(d.Corrected), "a missing grant is never re-granted")
	sev, _, _, open := findingOf(t, c.super, DriftFindingCategory, p.BrokerRole())
	require.True(t, open)
	require.Equal(t, "warning", sev)
}

func TestDrift_MissingRoleIsNarrowing(t *testing.T) {
	c, p := driftCluster(t)
	c.dropIfPresent(t, p.LoginRole())
	rep, err := c.driftRunner(t).ReconcileDrift(context.Background(), Fence{})
	require.NoError(t, err)
	d, ok := driftOf(rep, p.LoginRole())
	require.True(t, ok, "%+v", rep)
	require.Contains(t, strings.Join(d.Narrowing, ";"), "missing")
	require.Equal(t, 0, len(d.Widening))
}

func TestDrift_FencedOffChangesNothing(t *testing.T) {
	c, p := driftCluster(t)
	table := c.ownTable(t)
	ctx := context.Background()
	_, err := c.admin.Exec(ctx, "GRANT SELECT ON "+table+" TO "+p.BrokerRole())
	require.NoError(t, err)
	fence := takeLease(t, c.super, uniq("scope"))
	fence.Epoch = 1
	_, err = c.driftRunner(t).ReconcileDrift(ctx, fence)
	require.ErrorIs(t, err, ErrFenced)
	require.True(t, c.can(t, p.BrokerRole(), table, "SELECT"), "nothing revoked")
	require.Equal(t, 0, findingCount(t, c.super, DriftFindingCategory, p.BrokerRole(),
		"open"))
}

func TestDrift_NoBaselineFailsClosedForThatDatabase(t *testing.T) {
	c, _ := driftCluster(t)
	ctx := context.Background()
	_, err := c.super.Exec(ctx, "DELETE FROM sage.guard_public_baseline")
	require.NoError(t, err)
	rep, err := c.driftRunner(t).ReconcileDrift(ctx, Fence{})
	require.NoError(t, err)
	joined := ""
	for k, v := range rep.Failed {
		joined += k + ": " + v + "\n"
	}
	require.Contains(t, joined, "baseline")
}
