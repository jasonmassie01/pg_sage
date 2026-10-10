package grants

import (
	"context"
	"errors"
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

// G1-02 from the grant side: the broker role's effective privileges over
// the catalog (pg_catalog and information_schema excluded) equal its
// registry grants plus what PUBLIC holds, before and after grants and
// after their revoke.
func TestEffectivePrivileges_EqualGrantsPlusPublic(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	check := func(stage string) {
		got := effective(t, f, "role", f.p.BrokerRole())
		want := union(registry(t, f), effective(t, f, "public", ""))
		require.Equal(t, want, got, stage)
	}
	check("before any grant")
	res, err := f.manager.Grant(ctx, f.request())
	require.NoError(t, err)
	check("after a prod grant")
	f.target.Env = envbind.EnvDev
	_, err = f.manager.Grant(ctx, f.request())
	require.NoError(t, err)
	check("after a dev grant")
	f.expireNow(t, grantIDs(res.Grants)...)
	_, err = f.manager.ExpireDue(ctx, f.target, Fence{}, 100)
	require.NoError(t, err)
	check("after expiry")
}

// effectiveSQL lists relation columns and schemas in user schemas a role
// (or PUBLIC, with role 'public') can SELECT or USAGE.
const effectiveSQL = `SELECT 'column:' || n.nspname || '.' || c.relname || '.' || a.attname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
WHERE c.relkind IN ('r', 'v', 'm', 'p', 'f')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname !~ '^pg_toast'
  AND has_column_privilege($1, c.oid, a.attnum, 'SELECT')
UNION ALL
SELECT 'schema:' || n.nspname FROM pg_namespace n
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname !~ '^pg_'
  AND has_schema_privilege($1, n.oid, 'USAGE')`

func effective(t *testing.T, f *fixture, kind, role string) map[string]bool {
	t.Helper()
	if kind == "public" {
		role = "public"
	}
	rows, err := f.super.Query(context.Background(), effectiveSQL, role)
	require.NoError(t, err)
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	out := map[string]bool{}
	for _, s := range list {
		out[s] = true
	}
	return out
}

func registry(t *testing.T, f *fixture) map[string]bool {
	t.Helper()
	page, err := List(context.Background(), f.super, Filter{PrincipalID: f.p.ID,
		State: StateActive, Limit: 200})
	require.NoError(t, err)
	out := map[string]bool{}
	for _, g := range page.Items {
		if g.ObjectKind == KindSchema {
			out["schema:"+g.ObjectName] = true
			continue
		}
		for _, c := range g.Columns {
			out["column:"+g.ObjectName+"."+c] = true
		}
	}
	return out
}

func union(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
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
		"limit 201": {PrincipalID: f.p.ID, Limit: 201},
		"bad state": {PrincipalID: f.p.ID, Limit: 1, State: "gone"},
		"bad cursor": {PrincipalID: f.p.ID, Limit: 1, Cursor: "abc"}} {
		_, err := List(ctx, f.super, bad)
		require.ErrorIs(t, err, agentguard.ErrInvalid, name)
	}
}
