package startup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Query-text visibility is pg_read_all_stats membership with INHERIT
// (what pg_stat_statements itself tests), not "some row has a non-NULL
// query": other roles' rows read '<insufficient privilege>', not NULL.

func dbCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func adminPool(t *testing.T, dsn string, params map[string]string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 2
	for k, v := range params {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(dbCtx(t), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testRole creates a NOLOGIN role (dropped at cleanup) with the attributes
// given and returns its name.
func testRole(t *testing.T, admin *pgxpool.Pool, attrs string) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random role suffix: %v", err)
	}
	role := "sage_fl08_" + hex.EncodeToString(b)
	if _, err := admin.Exec(dbCtx(t), "CREATE ROLE "+role+" NOLOGIN "+attrs); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+role); err != nil {
			t.Errorf("drop role %s: %v", role, err)
		}
	})
	return role
}

// poolAs is a pool whose sessions run as role (SET ROLE), so current_user
// is role exactly as for a login of that role.
func poolAs(t *testing.T, dsn, role string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 2
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(dbCtx(t), cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// hiddenRows counts the entries pool's role sees as '<insufficient
// privilege>': the fixture's proof that the role really cannot read them.
func hiddenRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(dbCtx(t), `SELECT count(*)::int FROM pg_stat_statements
		WHERE query = '<insufficient privilege>'`).Scan(&n); err != nil {
		t.Fatalf("count hidden rows: %v", err)
	}
	return n
}

func runChecksAs(t *testing.T, pool *pgxpool.Pool) *CheckResult {
	t.Helper()
	got, err := RunChecks(dbCtx(t), pool)
	if err != nil {
		t.Fatalf("RunChecks: %v", err)
	}
	return got
}

func TestQueryTextVisible_NonMemberSeesHiddenText(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	admin := adminPool(t, dsn, nil)
	role := testRole(t, admin, "")
	if _, err := admin.Exec(dbCtx(t), "SELECT 'fl08 other role' AS marker"); err != nil {
		t.Fatalf("seed another role's statement: %v", err)
	}
	as := poolAs(t, dsn, role)
	if hiddenRows(t, as) == 0 {
		t.Fatal("fixture: the non-member sees no '<insufficient privilege>' rows")
	}
	got := runChecksAs(t, as)
	if got.QueryTextVisible {
		t.Fatal("QueryTextVisible = true for a role without pg_read_all_stats")
	}
}

func TestQueryTextVisible_MemberAfterReset(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	admin := adminPool(t, dsn, nil)
	role := testRole(t, admin, "")
	if _, err := admin.Exec(dbCtx(t), "GRANT pg_read_all_stats TO "+role); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := testdb.ResetStatements(dbCtx(t), admin); err != nil {
		t.Fatalf("reset: %v", err)
	}
	as := poolAs(t, dsn, role)
	if n := hiddenRows(t, as); n != 0 {
		t.Fatalf("fixture: a member sees %d hidden rows", n)
	}
	if got := runChecksAs(t, as); !got.QueryTextVisible {
		t.Fatal("QueryTextVisible = false for a pg_read_all_stats member")
	}
}

// Boundary: a NOINHERIT member holds the role but not its privileges.
// pg_stat_statements hides other roles' text from it on PostgreSQL 15+,
// which tests the role's privileges; PostgreSQL 14 tests membership alone
// and shows it every text. The check must follow the server.
func TestQueryTextVisible_NoInheritMemberFollowsServer(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	admin := adminPool(t, dsn, nil)
	role := testRole(t, admin, "NOINHERIT")
	if _, err := admin.Exec(dbCtx(t), "GRANT pg_read_all_stats TO "+role); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := admin.Exec(dbCtx(t), "SELECT 'fl08 noinherit' AS marker"); err != nil {
		t.Fatalf("seed another role's statement: %v", err)
	}
	var version int
	if err := admin.QueryRow(dbCtx(t),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	want := version < 150000
	as := poolAs(t, dsn, role)
	if hidden := hiddenRows(t, as); (hidden == 0) != want {
		t.Fatalf("fixture: PG %d NOINHERIT member sees %d hidden rows; want visible=%v",
			version, hidden, want)
	}
	if got := runChecksAs(t, as); got.QueryTextVisible != want {
		t.Fatalf("PG %d: QueryTextVisible = %v for a NOINHERIT member, want %v",
			version, got.QueryTextVisible, want)
	}
}

// Nil/empty: right after a reset (pg_sage's own reads are the only rows)
// the answer depends only on the role.
func TestQueryTextVisible_EmptyViewDependsOnRole(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	admin := adminPool(t, dsn, nil)
	member := testRole(t, admin, "")
	other := testRole(t, admin, "")
	if _, err := admin.Exec(dbCtx(t), "GRANT pg_read_all_stats TO "+member); err != nil {
		t.Fatalf("grant: %v", err)
	}
	for _, c := range []struct {
		role string
		want bool
	}{{member, true}, {other, false}} {
		if _, err := testdb.ResetStatements(dbCtx(t), admin); err != nil {
			t.Fatalf("reset: %v", err)
		}
		got, err := checkQueryTextVisible(dbCtx(t), poolAs(t, dsn, c.role))
		if err != nil || got != c.want {
			t.Errorf("role %s after reset: visible %v, %v; want %v", c.role, got, err, c.want)
		}
	}
}

// The extension in a schema outside the search_path: the checks find it
// through pg_extension and read it qualified, also under a name that needs
// quoting.
func TestRunChecks_ExtensionOutsideSearchPath(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "pgssschema")
	admin := adminPool(t, dsn, nil)
	for _, stmt := range []string{
		"DROP EXTENSION IF EXISTS pg_stat_statements",
		"CREATE SCHEMA ext",
		"CREATE EXTENSION pg_stat_statements SCHEMA ext",
		`CREATE SCHEMA "Odd ""S"""`,
	} {
		if _, err := admin.Exec(dbCtx(t), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	pool := adminPool(t, dsn, map[string]string{"search_path": "public"})
	assertSchemaChecks(t, pool, "ext")
	if _, err := admin.Exec(dbCtx(t),
		`ALTER EXTENSION pg_stat_statements SET SCHEMA "Odd ""S"""`); err != nil {
		t.Fatalf("move extension: %v", err)
	}
	assertSchemaChecks(t, pool, `Odd "S"`)
}

func assertSchemaChecks(t *testing.T, pool *pgxpool.Pool, schema string) {
	t.Helper()
	got := runChecksAs(t, pool)
	if got.StatementsSchema != schema {
		t.Errorf("StatementsSchema = %q, want %q", got.StatementsSchema, schema)
	}
	if !got.HasWALColumns || !got.HasPlanTimeColumns {
		t.Errorf("schema %q: WAL %v plan %v, want both true", schema, got.HasWALColumns,
			got.HasPlanTimeColumns)
	}
	if len(got.CapabilityErrors) != 0 {
		t.Errorf("schema %q: CapabilityErrors = %v", schema, got.CapabilityErrors)
	}
}
