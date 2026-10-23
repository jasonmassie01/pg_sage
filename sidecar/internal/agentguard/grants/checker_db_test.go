package grants

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// The grant-side D5 (decide.ObjectChecker) and D10 (decide.Leases): a
// brokered request may touch only columns an unexpired grant covers, never
// a secret one, never an unclassified one in stage/prod, and never an
// object whose revoke left another grantor's residue. G1-02: an agent's
// effective privileges are its grants plus the PUBLIC baseline.

var (
	_ decide.ObjectChecker = (*Checker)(nil)
	_ decide.Leases        = (*Checker)(nil)
)

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func objects(schema string, cols ...string) []decide.Object {
	var out []decide.Object
	for _, c := range cols {
		out = append(out, decide.Object{Schema: schema, Relation: "orders", Column: c})
	}
	return out
}

func (f *fixture) checker() *Checker {
	return &Checker{Resolve: func(_ context.Context, name string) (Target, error) {
		if name != f.db {
			return Target{}, envbind.ErrUnknownDatabase
		}
		return f.target, nil
	}}
}

func (f *fixture) principalNow(t *testing.T) agentguard.Principal {
	t.Helper()
	p, err := f.store.Get(context.Background(), f.p.ID)
	require.NoError(t, err)
	return p
}

func TestLeaseActive_FollowsTheRegistry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.checker()
	g := f.grantID(t)
	ok, err := c.LeaseActive(ctx, f.p.ID, itoa(g.ID), f.db)
	require.NoError(t, err)
	require.True(t, ok)
	other := f.principal(t, agentguard.EnvProd, true)
	ok, err = c.LeaseActive(ctx, other.ID, itoa(g.ID), f.db)
	require.NoError(t, err)
	require.False(t, ok, "another principal's grant is no lease")
	// Expired but not yet reconciled: the database grant still exists, the
	// lease does not (D10).
	f.expireNow(t, g.ID)
	require.True(t, f.can(t, f.p.BrokerRole(), "id"))
	ok, err = c.LeaseActive(ctx, f.p.ID, itoa(g.ID), f.db)
	require.NoError(t, err)
	require.False(t, ok)
	for _, bad := range []string{"", "x", "-1", "0", "99999999999999999999"} {
		ok, err = c.LeaseActive(ctx, f.p.ID, bad, f.db)
		require.False(t, ok, bad)
		require.True(t, err == nil || errors.Is(err, agentguard.ErrInvalid), bad)
	}
	_, err = c.LeaseActive(ctx, f.p.ID, itoa(g.ID), "nosuchdb")
	require.ErrorIs(t, err, envbind.ErrUnknownDatabase)
}

func TestCheckObjects_CoveredColumnsOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.checker()
	p := f.principalNow(t)
	err := c.CheckObjects(ctx, p, f.db, envbind.EnvProd, objects(f.schema, "id"))
	denied(t, err, decide.ReasonClassification) // nothing granted yet
	f.grantID(t)
	require.NoError(t, c.CheckObjects(ctx, p, f.db, envbind.EnvProd, objects(f.schema, "id")))
	require.NoError(t, c.CheckObjects(ctx, p, f.db, envbind.EnvProd,
		[]decide.Object{{Schema: f.schema, Relation: "orders"}}), "the relation as a whole")
	require.NoError(t, c.CheckObjects(ctx, p, f.db, envbind.EnvProd, nil), "nothing touched")
	for col, reason := range map[string]agentguard.Reason{
		"secret_token": decide.ReasonClassification, "note": decide.ReasonClassification,
		"email": decide.ReasonClassification, "amount": decide.ReasonClassification,
		"nosuch": decide.ReasonClassification,
	} {
		err := c.CheckObjects(ctx, p, f.db, envbind.EnvProd, objects(f.schema, "id", col))
		denied(t, err, reason)
	}
	err = c.CheckObjects(ctx, p, f.db, envbind.EnvProd, []decide.Object{{Schema: f.schema,
		Relation: "missing", Column: "id"}})
	denied(t, err, decide.ReasonClassification)
	err = c.CheckObjects(ctx, p, "nosuchdb", envbind.EnvProd, objects(f.schema, "id"))
	require.ErrorIs(t, err, envbind.ErrUnknownDatabase)
}

// A secret column stays denied even when a superuser granted it outside
// pg_sage, and an expired lease denies even while the privilege remains.
func TestCheckObjects_SecretAndExpired(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.checker()
	f.target.Env = envbind.EnvDev
	res, err := f.manager.Grant(ctx, f.request())
	require.NoError(t, err)
	p := f.principalNow(t)
	require.NoError(t, c.CheckObjects(ctx, p, f.db, envbind.EnvDev,
		objects(f.schema, "id", "note")), "dev: unclassified granted and usable")
	err = c.CheckObjects(ctx, p, f.db, envbind.EnvProd, objects(f.schema, "note"))
	denied(t, err, decide.ReasonClassification) // prod treatment refuses unclassified
	err = c.CheckObjects(ctx, p, f.db, envbind.EnvDev, objects(f.schema, "secret_token"))
	denied(t, err, decide.ReasonClassification)
	f.expireNow(t, grantIDs(res.Grants)...)
	err = c.CheckObjects(ctx, p, f.db, envbind.EnvDev, objects(f.schema, "id"))
	denied(t, err, decide.ReasonLeaseExpired)
}

// G1-02 from the grant side: the broker role's effective privileges are
// exactly its registry grants plus the PUBLIC baseline the preflight
// recorded (core's CheckEffectivePrivileges), before and after grants and
// after their expiry; a privilege granted outside the registry is excess.
func TestEffectivePrivileges_EqualGrantsPlusPublic(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := agentguard.Preflight(ctx, f.super, nil)
	require.NoError(t, err)
	check := func(stage string, wantExcess int) {
		privs, err := RegistryPrivileges(ctx, f.super, f.p.ID)
		require.NoError(t, err)
		rep, err := agentguard.CheckEffectivePrivileges(ctx, f.super, f.p.BrokerRole(),
			privs)
		require.NoError(t, err)
		require.Len(t, rep.Excess, wantExcess, "%s: %+v", stage, rep.Excess)
		eff, err := agentguard.EffectivePrivileges(ctx, f.super, f.p.BrokerRole())
		require.NoError(t, err)
		held := map[string]bool{}
		for _, p := range eff {
			held[fmt.Sprintf("%s/%d/%d/%s", p.Kind, p.OID, p.Attnum, p.Privilege)] = true
		}
		for _, p := range privs {
			key := fmt.Sprintf("%s/%d/%d/%s", p.Kind, p.OID, p.Attnum, p.Privilege)
			require.True(t, held[key], "%s: registry privilege %s not held", stage, key)
		}
	}
	check("before any grant", 0)
	res, err := f.manager.Grant(ctx, f.request())
	require.NoError(t, err)
	check("after a prod grant", 0)
	f.target.Env = envbind.EnvDev
	_, err = f.manager.Grant(ctx, f.request())
	require.NoError(t, err)
	check("after a dev grant", 0)
	f.expireNow(t, grantIDs(res.Grants)...)
	_, err = f.manager.ExpireDue(ctx, f.target, Fence{}, 100)
	require.NoError(t, err)
	check("after expiry", 0)
	f.exec1(t, "GRANT SELECT (amount) ON "+pgx.Identifier{f.schema, "orders"}.Sanitize()+
		" TO "+f.p.BrokerRole())
	check("a grant outside the registry", 1)
}

func TestRegistryPrivileges_Shape(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	none, err := RegistryPrivileges(ctx, f.super, f.p.ID)
	require.NoError(t, err)
	require.Empty(t, none)
	g := f.grantID(t)
	privs, err := RegistryPrivileges(ctx, f.super, f.p.ID)
	require.NoError(t, err)
	require.Len(t, privs, 2)
	kinds := map[string]agentguard.Privilege{}
	for _, p := range privs {
		kinds[p.Kind] = p
	}
	require.Equal(t, g.ObjectOID, kinds["column"].OID)
	require.Equal(t, int16(1), kinds["column"].Attnum) // id is the first column
	require.Equal(t, "SELECT", kinds["column"].Privilege)
	require.Equal(t, g.SchemaOID, kinds["schema"].OID)
	require.Equal(t, "USAGE", kinds["schema"].Privilege)
	_, err = RegistryPrivileges(ctx, f.super, "bob")
	require.ErrorIs(t, err, agentguard.ErrInvalid)
}

func TestList_PagesAndFilters(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 3; i++ {
		ids = append(ids, f.grantID(t).ID)
	}
	page, err := List(ctx, f.super, Filter{PrincipalID: f.p.ID, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.NotEqual(t, "", page.NextCursor)
	rest, err := List(ctx, f.super, Filter{PrincipalID: f.p.ID, Limit: 200,
		Cursor: page.NextCursor})
	require.NoError(t, err)
	require.Len(t, rest.Items, 2) // one relation + the schema row
	require.Equal(t, "", rest.NextCursor)
	var seen []int64
	for _, g := range append(page.Items, rest.Items...) {
		seen = append(seen, g.ID)
	}
	sort.Slice(seen, func(i, j int) bool { return seen[i] > seen[j] })
	require.Equal(t, seen[0] > seen[3], true, "newest first")
	_, err = f.manager.Revoke(ctx, RevokeRequest{Target: f.target, GrantID: ids[0],
		Cause: CauseOperator, ApprovedBy: 1})
	require.NoError(t, err)
	revoked, err := List(ctx, f.super, Filter{PrincipalID: f.p.ID, State: StateRevoked,
		Limit: 200})
	require.NoError(t, err)
	require.Len(t, revoked.Items, 1)
	require.Equal(t, ids[0], revoked.Items[0].ID)
	for name, bad := range map[string]Filter{"limit 0": {PrincipalID: f.p.ID},
		"limit 201":  {PrincipalID: f.p.ID, Limit: 201},
		"bad state":  {PrincipalID: f.p.ID, Limit: 1, State: "gone"},
		"bad cursor": {PrincipalID: f.p.ID, Limit: 1, Cursor: "abc"}} {
		_, err := List(ctx, f.super, bad)
		require.ErrorIs(t, err, agentguard.ErrInvalid, name)
	}
}
