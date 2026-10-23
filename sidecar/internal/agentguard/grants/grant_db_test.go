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
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// guard_grant (§6.3, §6.6, §6.7): operator-approved, column-listed,
// time-boxed, scoped to the principal's broker role; prod lists only
// columns classified clean (G1 rule); pg_sage grants only what it holds
// WITH GRANT OPTION (G1-09) and refuses when PUBLIC can CREATE in the
// schema (G1-16).

func TestGrant_ProdListsOnlyConfirmedCleanColumns(t *testing.T) {
	f := newFixture(t)
	res, err := f.manager.Grant(context.Background(), f.request())
	require.NoError(t, err)
	require.True(t, res.ActionID > 0, "action id")
	g := relationGrant(t, res.Grants)
	require.Equal(t, []string{"id"}, g.Columns)
	require.Equal(t, []string{"SELECT"}, g.Privileges)
	require.Equal(t, f.adminRol, g.Grantor)
	require.Equal(t, StateActive, g.State)
	require.Equal(t, LaneBroker, g.Lane)
	require.Equal(t, f.p.ID, g.PrincipalID)
	require.Equal(t, f.target.ID, g.DatabaseID)
	require.Equal(t, f.schema+".orders", g.ObjectName)
	require.Equal(t, res.ActionID, g.GrantActionID)
	d := g.ExpiresAt.Sub(g.GrantedAt)
	require.True(t, d > 59*time.Minute && d < 61*time.Minute, "expiry %v", d)
	reasons := map[string]string{}
	for _, x := range res.Excluded {
		reasons[x.Column] = x.Reason
	}
	require.Equal(t, map[string]string{"email": "pii_without_unmask",
		"secret_token": "secret", "note": "unclassified", "region": "pending",
		"amount": "grantor_lacks_privilege"}, reasons)
	broker := f.p.BrokerRole()
	require.True(t, f.can(t, broker, "id"), "id granted")
	for _, col := range []string{"email", "secret_token", "note", "region", "amount"} {
		require.False(t, f.can(t, broker, col), col+" must not be granted")
	}
	require.True(t, f.schemaUsage(t, broker), "schema USAGE comes with the first grant")
	require.False(t, f.can(t, f.p.LoginRole(), "id"), "the direct-lane role gets nothing")
	require.Len(t, res.Grants, 2) // the relation and its schema USAGE
	require.Len(t, f.actions(t, executor.ActionTypeGuardGrant), 1)
	sql := f.actions(t, executor.ActionTypeGuardGrant)[0]
	require.True(t, strings.Contains(sql, `GRANT SELECT ("id") ON TABLE`), sql)
}

func TestGrant_DevGrantsUnclassifiedButNeverSecret(t *testing.T) {
	f := newFixture(t)
	f.target.Env = envbind.EnvDev
	res, err := f.manager.Grant(context.Background(), f.request())
	require.NoError(t, err)
	g := relationGrant(t, res.Grants)
	require.Equal(t, []string{"id", "email", "note", "region"}, g.Columns)
	require.False(t, f.can(t, f.p.BrokerRole(), "secret_token"), "secret never granted")
}

func TestGrant_UnknownEnvironmentIsProd(t *testing.T) {
	f := newFixture(t)
	f.target.Env = envbind.Env("qa")
	res, err := f.manager.Grant(context.Background(), f.request())
	require.NoError(t, err)
	require.Equal(t, []string{"id"}, relationGrant(t, res.Grants).Columns)
}

func TestGrant_ExplicitExcludedColumnIsDenied(t *testing.T) {
	f := newFixture(t)
	for _, col := range []string{"secret_token", "note", "email", "region"} {
		_, err := f.manager.Grant(context.Background(), f.request("id", col))
		d := denied(t, err, decide.ReasonClassification)
		require.True(t, strings.Contains(d.Detail, col), d.Detail)
	}
	require.False(t, f.can(t, f.p.BrokerRole(), "id"), "a denied request grants nothing")
	require.Len(t, f.actions(t, executor.ActionTypeGuardGrant), 0)
}

func TestGrant_GrantorLacksPrivilege_ExactStatement(t *testing.T) {
	f := newFixture(t)
	_, err := f.manager.Grant(context.Background(), f.request("amount"))
	d := denied(t, err, agentguard.ReasonGrantorLacksPrivilege)
	want := `GRANT SELECT ("amount") ON TABLE ` + pgx.Identifier{f.schema, "orders"}.Sanitize() +
		` TO ` + pgx.Identifier{f.adminRol}.Sanitize() + ` WITH GRANT OPTION;`
	require.Equal(t, want, d.Fix)
	require.False(t, f.can(t, f.p.BrokerRole(), "amount"))
}

// Owner-role membership is never used to grant (it gives DROP): pg_sage
// a member of the owner still lacks the grant option itself.
func TestGrant_OwnerMembershipDoesNotCount(t *testing.T) {
	f := newFixture(t)
	f.exec1(t, "GRANT "+f.owner+" TO "+f.adminRol)
	_, err := f.manager.Grant(context.Background(), f.request("amount"))
	denied(t, err, agentguard.ReasonGrantorLacksPrivilege)
	require.False(t, f.can(t, f.p.BrokerRole(), "amount"))
}

func TestGrant_SchemaUsageWithoutGrantOption(t *testing.T) {
	f := newFixture(t)
	s := pgx.Identifier{f.schema}.Sanitize()
	f.exec1(t, "REVOKE GRANT OPTION FOR USAGE ON SCHEMA "+s+" FROM "+f.adminRol)
	_, err := f.manager.Grant(context.Background(), f.request("id"))
	d := denied(t, err, agentguard.ReasonGrantorLacksPrivilege)
	require.Equal(t, "GRANT USAGE ON SCHEMA "+s+" TO "+pgx.Identifier{f.adminRol}.Sanitize()+
		" WITH GRANT OPTION;", d.Fix)
}

func TestGrant_PublicCreateOnSchemaIsRefused(t *testing.T) {
	f := newFixture(t)
	s := pgx.Identifier{f.schema}.Sanitize()
	f.exec1(t, "GRANT CREATE ON SCHEMA "+s+" TO PUBLIC")
	_, err := f.manager.Grant(context.Background(), f.request("id"))
	d := denied(t, err, agentguard.ReasonPublicCreate)
	require.Equal(t, "REVOKE CREATE ON SCHEMA "+s+" FROM PUBLIC;", d.Fix)
	require.False(t, f.can(t, f.p.BrokerRole(), "id"))
	f.exec1(t, "REVOKE CREATE ON SCHEMA "+s+" FROM PUBLIC")
	_, err = f.manager.Grant(context.Background(), f.request("id"))
	require.NoError(t, err)
}

func TestGrant_InvalidRequests(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cases := map[string]func(*GrantRequest){
		"bad principal":   func(r *GrantRequest) { r.PrincipalID = "nope" },
		"no objects":      func(r *GrantRequest) { r.Objects = nil },
		"zero duration":   func(r *GrantRequest) { r.Duration = 0 },
		"negative":        func(r *GrantRequest) { r.Duration = -time.Minute },
		"under a minute":  func(r *GrantRequest) { r.Duration = 59 * time.Second },
		"over max":        func(r *GrantRequest) { r.Duration = 4*time.Hour + time.Minute },
		"unqualified":     func(r *GrantRequest) { r.Objects[0].Object = "orders" },
		"three parts":     func(r *GrantRequest) { r.Objects[0].Object = "a.b.c" },
		"no executor":     func(r *GrantRequest) { r.Target.Executor = nil },
		"no pool":         func(r *GrantRequest) { r.Target.Pool = nil },
		"no database id":  func(r *GrantRequest) { r.Target.ID = "" },
		"injection table": func(r *GrantRequest) { r.Objects[0].Object = "x.y; DROP TABLE z" },
		"dup column": func(r *GrantRequest) {
			r.Objects[0].Columns = []string{"id", "id"}
		},
		"too many objects": func(r *GrantRequest) {
			for i := 0; i < 60; i++ {
				r.Objects = append(r.Objects, r.Objects[0])
			}
		},
	}
	for name, mut := range cases {
		req := f.request("id")
		mut(&req)
		_, err := f.manager.Grant(ctx, req)
		if !errors.Is(err, agentguard.ErrInvalid) && !errors.Is(err, agentguard.ErrNotFound) {
			t.Errorf("%s: want ErrInvalid or ErrNotFound, got %v", name, err)
		}
	}
	require.False(t, f.can(t, f.p.BrokerRole(), "id"))
}

func TestGrant_UnknownObjectAndColumn(t *testing.T) {
	f := newFixture(t)
	req := f.request("id")
	req.Objects[0].Object = f.schema + ".missing"
	_, err := f.manager.Grant(context.Background(), req)
	require.ErrorIs(t, err, agentguard.ErrNotFound)
	_, err = f.manager.Grant(context.Background(), f.request("nosuchcol"))
	require.ErrorIs(t, err, agentguard.ErrNotFound)
}

func TestGrant_DurationBoundaryIsInclusive(t *testing.T) {
	f := newFixture(t)
	req := f.request("id")
	req.Duration = 4 * time.Hour
	res, err := f.manager.Grant(context.Background(), req)
	require.NoError(t, err)
	g := relationGrant(t, res.Grants)
	require.True(t, g.ExpiresAt.Sub(g.GrantedAt) == 4*time.Hour, "expiry on the DB clock")
	req.Duration = time.Minute
	_, err = f.manager.Grant(context.Background(), req)
	require.NoError(t, err)
}

func TestGrant_CapabilityOtherThanReadIsRefused(t *testing.T) {
	f := newFixture(t)
	for _, c := range []string{"write_insert", "ddl_additive", "", "READ"} {
		req := f.request("id")
		req.Capability = c
		_, err := f.manager.Grant(context.Background(), req)
		if _, ok := agentguard.IsDenied(err); !ok && !errors.Is(err, agentguard.ErrInvalid) {
			t.Errorf("capability %q: got %v", c, err)
		}
	}
	require.False(t, f.can(t, f.p.BrokerRole(), "id"))
}

func TestGrant_NeedsOperatorApproval(t *testing.T) {
	f := newFixture(t)
	req := f.request("id")
	req.Approval = agentguard.Approval{}
	_, err := f.manager.Grant(context.Background(), req)
	require.ErrorIs(t, err, agentguard.ErrApprovalRequired)
}

func TestGrant_PrincipalStateGates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	unsponsored := f.principal(t, agentguard.EnvProd, false)
	req := f.request("id")
	req.PrincipalID = unsponsored.ID
	_, err := f.manager.Grant(ctx, req)
	denied(t, err, agentguard.ReasonUnsponsored)

	devOnly := f.principal(t, agentguard.EnvDev, true)
	req.PrincipalID = devOnly.ID
	_, err = f.manager.Grant(ctx, req)
	denied(t, err, decide.ReasonEnvCeiling)
	req.Target.Env = envbind.EnvDev
	_, err = f.manager.Grant(ctx, req)
	require.NoError(t, err, "dev target within a dev ceiling")

	_, err = f.store.SetStatus(ctx, f.p.ID, agentguard.StatusFrozen, "test")
	require.NoError(t, err)
	_, err = f.manager.Grant(ctx, f.request("id"))
	denied(t, err, agentguard.ReasonFrozen)
	require.False(t, f.can(t, f.p.BrokerRole(), "id"))
}

func TestGrant_NoRolesOnCluster(t *testing.T) {
	f := newFixture(t)
	p, err := f.store.Create(context.Background(), agentguard.CreateRequest{
		Name: "noroles-" + strings.ReplaceAll(uniq("x"), "_", "-"), SponsorUserID: f.p.SponsorUserID,
		Profile: "readonly-analyst", EnvCeiling: agentguard.EnvProd, CreatedBy: "a@b.c"})
	require.NoError(t, err)
	req := f.request("id")
	req.PrincipalID = p.ID
	_, err = f.manager.Grant(context.Background(), req)
	denied(t, err, ReasonNoRoles)
}

// Acting needs trust.level advisory or higher: at observation, or during
// an emergency stop, a grant is withheld and nothing changes.
func TestGrant_WithheldAtObservationAndDuringStop(t *testing.T) {
	f := newFixture(t)
	for name, mut := range map[string]func(*policy.RuntimeState){
		"observation": func(r *policy.RuntimeState) { r.TrustLevel = policy.TrustObservation },
		"stop": func(r *policy.RuntimeState) {
			r.TrustLevel, r.EmergencyStop = policy.TrustAdvisory, true
		},
		"disabled": func(r *policy.RuntimeState) {
			r.EmergencyStop, r.ExecutorEnabled = false, false
		},
	} {
		f.setRuntime(mut)
		_, err := f.manager.Grant(context.Background(), f.request("id"))
		require.ErrorIs(t, err, executor.ErrActionWithheld, name)
		require.False(t, f.can(t, f.p.BrokerRole(), "id"), name)
	}
	var n int
	require.NoError(t, f.super.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.guard_grants WHERE principal_id = $1", f.p.ID).Scan(&n))
	require.Equal(t, 0, n)
}

// A second table in the same schema shares one schema USAGE row; its
// expiry is extended to the later grant.
func TestGrant_SchemaUsageIsReferenceCounted(t *testing.T) {
	f := newFixture(t)
	s := pgx.Identifier{f.schema}.Sanitize()
	f.exec1(t, "CREATE TABLE "+s+".items (id int)")
	f.exec1(t, "GRANT SELECT ON "+s+".items TO "+f.adminRol+" WITH GRANT OPTION")
	ctx := context.Background()
	first, err := f.manager.Grant(ctx, f.request("id"))
	require.NoError(t, err)
	req := f.request()
	req.Objects = []ObjectRequest{{Object: f.schema + ".items"}}
	req.Duration = 2 * time.Hour
	f.target.Env = envbind.EnvDev
	req.Target = f.target
	second, err := f.manager.Grant(ctx, req)
	require.NoError(t, err)
	require.Len(t, second.Grants, 1, "no second schema row")
	var rows int
	var exp time.Time
	require.NoError(t, f.super.QueryRow(ctx, `SELECT count(*), max(expires_at)
		FROM sage.guard_grants WHERE principal_id = $1 AND object_kind = 'schema'
		AND revoked_at IS NULL`, f.p.ID).Scan(&rows, &exp))
	require.Equal(t, 1, rows)
	require.True(t, exp.Equal(relationGrant(t, second.Grants).ExpiresAt) &&
		exp.After(relationGrant(t, first.Grants).ExpiresAt), "schema row follows the latest")
}

// Two operators granting at once: both succeed, one schema row.
func TestGrant_ConcurrentGrantsSerialize(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.manager.Grant(context.Background(), f.request("id"))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, i)
	}
	var schemas, rels int
	require.NoError(t, f.super.QueryRow(context.Background(), `SELECT
		count(*) FILTER (WHERE object_kind = 'schema'),
		count(*) FILTER (WHERE object_kind = 'relation')
		FROM sage.guard_grants WHERE principal_id = $1`, f.p.ID).Scan(&schemas, &rels))
	require.Equal(t, 1, schemas)
	require.Equal(t, 4, rels)
}
