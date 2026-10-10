package agentguard

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// privFixture is a schema with two tables and a plain agent-named role.
type privFixture struct {
	pool   *pgxpool.Pool
	schema string
	role   string
}

func newPrivFixture(t *testing.T) privFixture {
	t.Helper()
	pool := livePool(t)
	lockAgentRoles(t)
	ctx := context.Background()
	suffix := strings.ToLower(uniqName("p"))[2:]
	suffix = strings.NewReplacer("-", "").Replace(suffix)
	f := privFixture{pool: pool, schema: "g1priv_" + suffix}
	f.role = BrokerRoleName(f.schema) // an agent-named role, unregistered
	for _, s := range []string{
		"CREATE SCHEMA " + f.schema,
		"CREATE TABLE " + f.schema + ".open (id int, note text)",
		"CREATE TABLE " + f.schema + ".secret (id int, a text, b text)",
		"CREATE SEQUENCE " + f.schema + ".seq",
		"GRANT SELECT ON " + f.schema + ".open TO PUBLIC",
		"CREATE ROLE " + f.role + " NOLOGIN",
	} {
		_, err := pool.Exec(ctx, s)
		require.NoError(t, err, s)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+f.schema+" CASCADE")
		_, _ = pool.Exec(ctx, "DROP OWNED BY "+f.role)
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+f.role)
	})
	return f
}

func (f privFixture) oid(t *testing.T, rel string) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(context.Background(),
		"SELECT $1::regclass::oid", f.schema+"."+rel).Scan(&oid))
	return oid
}

func (f privFixture) schemaOID(t *testing.T) uint32 {
	t.Helper()
	var oid uint32
	require.NoError(t, f.pool.QueryRow(context.Background(),
		"SELECT oid FROM pg_namespace WHERE nspname = $1", f.schema).Scan(&oid))
	return oid
}

// guardGrants is what the grant registry would hold: USAGE on the schema
// and SELECT on column a of the secret table.
func (f privFixture) guardGrants(t *testing.T) []Privilege {
	return []Privilege{
		{Kind: "schema", OID: f.schemaOID(t), Name: f.schema, Privilege: "USAGE"},
		{Kind: "column", OID: f.oid(t, "secret"), Attnum: 2, Privilege: "SELECT"},
	}
}

func (f privFixture) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), sql)
	require.NoError(t, err, sql)
}

func hasPrivilege(ps []Privilege, kind string, oid uint32, priv string) bool {
	for _, p := range ps {
		if p.Kind == kind && p.OID == oid && p.Privilege == priv {
			return true
		}
	}
	return false
}

func TestEffectivePrivileges_EqualGrantsPlusPublicBaseline(t *testing.T) {
	f := newPrivFixture(t)
	ctx := context.Background()
	f.exec(t, "GRANT USAGE ON SCHEMA "+f.schema+" TO "+f.role)
	f.exec(t, "GRANT SELECT (a) ON "+f.schema+".secret TO "+f.role)
	res, err := Preflight(ctx, f.pool, []string{f.schema})
	require.NoError(t, err)
	require.Positive(t, res.Baseline)
	require.NoError(t, res.GrantsAllowed())
	eff, err := EffectivePrivileges(ctx, f.pool, f.role)
	require.NoError(t, err)
	require.True(t, hasPrivilege(eff, "relation", f.oid(t, "open"), "SELECT"),
		"PUBLIC's SELECT reaches the agent")
	require.True(t, hasPrivilege(eff, "column", f.oid(t, "secret"), "SELECT"))
	require.False(t, hasPrivilege(eff, "relation", f.oid(t, "secret"), "SELECT"))
	rep, err := CheckEffectivePrivileges(ctx, f.pool, f.role, f.guardGrants(t))
	require.NoError(t, err)
	require.Empty(t, rep.Excess, "grants plus the baseline cover everything")
	require.Equal(t, len(eff), rep.Effective)
}

func TestEffectivePrivileges_ReportsLeaks(t *testing.T) {
	f := newPrivFixture(t)
	ctx := context.Background()
	f.exec(t, "GRANT USAGE ON SCHEMA "+f.schema+" TO "+f.role)
	_, err := Preflight(ctx, f.pool, []string{f.schema})
	require.NoError(t, err)
	f.exec(t, "GRANT INSERT ON "+f.schema+".secret TO "+f.role)
	f.exec(t, "GRANT USAGE ON SEQUENCE "+f.schema+".seq TO "+f.role)
	rep, err := CheckEffectivePrivileges(ctx, f.pool, f.role, f.guardGrants(t))
	require.NoError(t, err)
	require.True(t, hasPrivilege(rep.Excess, "relation", f.oid(t, "secret"), "INSERT"))
	require.True(t, hasPrivilege(rep.Excess, "relation", f.oid(t, "seq"), "USAGE"))
	// The launching admin's privileges never reach an agent: a membership
	// that would bring them shows as excess (SAFE-ID-02).
	f.exec(t, "GRANT pg_read_all_data TO "+f.role)
	rep, err = CheckEffectivePrivileges(ctx, f.pool, f.role, f.guardGrants(t))
	require.NoError(t, err)
	require.True(t, hasPrivilege(rep.Excess, "relation", f.oid(t, "secret"), "SELECT"))
	f.exec(t, "REVOKE pg_read_all_data FROM "+f.role)
}

func TestEffectivePrivileges_NewPublicGrantAfterPreflightIsExcess(t *testing.T) {
	f := newPrivFixture(t)
	ctx := context.Background()
	f.exec(t, "GRANT USAGE ON SCHEMA "+f.schema+" TO "+f.role)
	_, err := Preflight(ctx, f.pool, []string{f.schema})
	require.NoError(t, err)
	f.exec(t, "GRANT UPDATE ON "+f.schema+".open TO PUBLIC")
	rep, err := CheckEffectivePrivileges(ctx, f.pool, f.role, f.guardGrants(t))
	require.NoError(t, err)
	require.True(t, hasPrivilege(rep.Excess, "relation", f.oid(t, "open"), "UPDATE"),
		"PUBLIC widened since the preflight")
	_, err = RecordPublicBaseline(ctx, f.pool)
	require.NoError(t, err)
	rep, err = CheckEffectivePrivileges(ctx, f.pool, f.role, f.guardGrants(t))
	require.NoError(t, err)
	require.Empty(t, rep.Excess, "re-recorded baseline")
}

func TestCheckEffectivePrivileges_NoBaselineFailsClosed(t *testing.T) {
	f := newPrivFixture(t)
	ctx := context.Background()
	f.exec(t, "DELETE FROM sage.guard_public_baseline")
	_, err := CheckEffectivePrivileges(ctx, f.pool, f.role, nil)
	require.ErrorIs(t, err, ErrNoBaseline)
	_, err = EffectivePrivileges(ctx, f.pool, "no_such_role_g1core")
	require.Error(t, err)
}

func TestPreflight_PublicCreateRefusesGrants(t *testing.T) {
	f := newPrivFixture(t)
	ctx := context.Background()
	f.exec(t, "GRANT CREATE ON SCHEMA "+f.schema+" TO PUBLIC")
	res, err := Preflight(ctx, f.pool, []string{f.schema})
	require.NoError(t, err)
	require.Equal(t, []string{f.schema}, res.PublicCreate)
	d, ok := IsDenied(res.GrantsAllowed())
	require.True(t, ok)
	require.Equal(t, ReasonPublicCreate, d.Reason)
	require.Equal(t, "REVOKE CREATE ON SCHEMA "+ident(f.schema)+" FROM PUBLIC;", d.Fix,
		"G1-16: the exact REVOKE")
	f.exec(t, "REVOKE CREATE ON SCHEMA "+f.schema+" FROM PUBLIC")
	res, err = Preflight(ctx, f.pool, []string{f.schema})
	require.NoError(t, err)
	require.Empty(t, res.PublicCreate)
	require.NoError(t, res.GrantsAllowed())
	// Every schema when no profile path is given: PostgreSQL 14 still
	// grants CREATE on public to PUBLIC; 15 and later do not.
	all, err := Preflight(ctx, f.pool, nil)
	require.NoError(t, err)
	publicCreate := false
	for _, s := range all.PublicCreate {
		publicCreate = publicCreate || s == "public"
	}
	require.Equal(t, serverVersionNum(t, f.pool) < 150000, publicCreate)
	_, err = Preflight(ctx, nil, nil)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestPreflight_DirectLaneBlockers(t *testing.T) {
	f := newPrivFixture(t)
	ctx := context.Background()
	f.exec(t, "CREATE FUNCTION "+f.schema+".definer() RETURNS int LANGUAGE sql "+
		"SECURITY DEFINER VOLATILE AS 'SELECT 1'")
	f.exec(t, "GRANT USAGE ON SCHEMA "+f.schema+" TO PUBLIC")
	res, err := Preflight(ctx, f.pool, []string{f.schema})
	require.NoError(t, err)
	require.Contains(t, res.PublicDefinerFunctions, f.schema+".definer()")
	require.Contains(t, res.PublicConnect, "postgres", "P2: PUBLIC CONNECT by default")
	require.NoError(t, res.GrantsAllowed(), "P2 and P3 refuse only the direct lane")
}
