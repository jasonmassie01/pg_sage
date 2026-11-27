package doctor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Concurrency: checks run sequentially by design, on one connection
// (MaxConns 1 in the CLI), so these tests run each check once per run.

func dbCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func onePool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// sqlChecks are the checks that run only SQL on the target.
var sqlChecks = []string{IDConnection, IDVersion, IDRoleGrants, IDExtensions,
	IDProvider, IDQueryText, IDClockSkew}

// Happy path (DB): a superuser on a local server passes every SQL check
// (HypoPG installed when the server ships it) and exits 0.
func TestDoctorDBSuperuserPassesEverySQLCheck(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "doctor_happy")
	pool := onePool(t, dsn)
	ctx := dbCtx(t)
	mustExec(t, pool, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	hypo := hypopgAvailable(t, pool)
	if hypo {
		mustExec(t, pool, "CREATE EXTENSION IF NOT EXISTS hypopg")
	}
	r := Run(ctx, Inputs{Target: pool})
	assertOrder(t, r)
	for _, id := range sqlChecks {
		c := mustCheck(t, r, id)
		want := StatusPass
		if id == IDExtensions && !hypo {
			want = StatusWarn // the image has no HypoPG to install
		}
		if c.Status != want {
			t.Errorf("%s = %+v, want %s", id, c, want)
		}
	}
	if r.Provider != "SM" || r.ExitCode() != 0 {
		t.Fatalf("provider %q exit %d, want SM and 0: %+v", r.Provider, r.ExitCode(), r)
	}
}

// Error propagation and state transition: a role without pg_monitor fails
// with the exact GRANT. Every other fix the report gives is run as written
// (they are copyable SQL); the check still fails on pg_monitor alone, and
// granting it flips the check to pass and the exit code from 1 to 0.
func TestDoctorDBMissingPgMonitorThenGranted(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "doctor_grants")
	admin := onePool(t, dsn)
	mustExec(t, admin, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	role, roleDSN := createLoginRole(t, admin, dsn)
	target := onePool(t, roleDSN)
	ctx := dbCtx(t)
	monitor := "GRANT pg_monitor TO " + role

	before := Run(ctx, Inputs{Target: target})
	grants := mustCheck(t, before, IDRoleGrants)
	if grants.Status != StatusFail || !strings.Contains(grants.Fix, monitor) {
		t.Fatalf("role check = %+v, want fail with %s", grants, monitor)
	}
	if qt := mustCheck(t, before, IDQueryText); qt.Status != StatusWarn ||
		!strings.Contains(qt.Fix, "pg_read_all_stats") {
		t.Fatalf("query text = %+v, want warn with the pg_read_all_stats fix", qt)
	}
	if p := mustCheck(t, before, IDProvider); p.Status != StatusUnknown {
		t.Fatalf("non-superuser on a plain server = %+v, want unknown provider", p)
	}
	for _, stmt := range strings.Split(grants.Fix, fixSeparator) {
		if stmt != monitor {
			mustExec(t, admin, stmt)
		}
	}
	mid := Run(ctx, Inputs{Target: target})
	if c := mustCheck(t, mid, IDRoleGrants); c.Status != StatusFail || c.Fix != monitor {
		t.Fatalf("role check with only pg_monitor missing = %+v, want fix %q", c, monitor)
	}
	if mid.ExitCode() != 1 {
		t.Fatalf("exit before the grant = %d, want 1", mid.ExitCode())
	}

	mustExec(t, admin, monitor)
	after := Run(ctx, Inputs{Target: target})
	if c := mustCheck(t, after, IDRoleGrants); c.Status != StatusPass {
		t.Fatalf("role check after the grant = %+v, want pass", c)
	}
	if c := mustCheck(t, after, IDQueryText); c.Status != StatusPass {
		t.Fatalf("query text after the grant = %+v, want pass", c)
	}
	if after.ExitCode() != 0 {
		t.Fatalf("exit after the grant = %d, want 0: %+v", after.ExitCode(), after.Checks)
	}
}

// Error propagation: pg_stat_statements not installed fails with the
// CREATE EXTENSION fix.
func TestDoctorDBMissingStatementsFails(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "doctor_nopss")
	pool := onePool(t, dsn)
	mustExec(t, pool, "DROP EXTENSION IF EXISTS pg_stat_statements")
	r := Run(dbCtx(t), Inputs{Target: pool})
	c := mustCheck(t, r, IDExtensions)
	if c.Status != StatusFail || !strings.Contains(c.Fix,
		"CREATE EXTENSION pg_stat_statements") {
		t.Fatalf("extensions = %+v, want fail with CREATE EXTENSION", c)
	}
}

// Boundary through the clock seam: the host clock 10 s and 40 s away from
// the server warns, then fails; an honest clock passes.
func TestDoctorDBClockSkewThroughTheSeam(t *testing.T) {
	pool := onePool(t, testdb.CreateDatabase(t, "doctor_clock"))
	ctx := dbCtx(t)
	for _, tc := range []struct {
		offset time.Duration
		want   Status
	}{{0, StatusPass}, {10 * time.Second, StatusWarn}, {-40 * time.Second, StatusFail}} {
		now := func() time.Time { return time.Now().Add(tc.offset) }
		c := mustCheck(t, Run(ctx, Inputs{Target: pool, Now: now}), IDClockSkew)
		if c.Status != tc.want || c.Detail == "" {
			t.Errorf("offset %s: clock = %+v, want %s", tc.offset, c, tc.want)
		}
	}
}

// Integration: a bootstrapped meta database passes, an empty one warns
// that the schema is not created yet, and nothing in either run writes.
func TestDoctorDBMetaSchema(t *testing.T) {
	ctx := dbCtx(t)
	empty := onePool(t, testdb.CreateDatabase(t, "doctor_meta_empty"))
	c := metaCheck(ctx, empty, nil, schema.MigrationStepNames())
	if c.Status != StatusWarn || !strings.Contains(c.Detail, "not created") {
		t.Fatalf("empty meta = %+v, want warn \"not created\"", c)
	}
	var tables int
	mustScan(t, empty, "SELECT count(*) FROM pg_catalog.pg_namespace WHERE nspname = 'sage'",
		&tables)
	if tables != 0 {
		t.Fatal("doctor created the sage schema on the meta database")
	}
	bootDSN := testdb.CreateDatabase(t, "doctor_meta_boot")
	boot, err := pgxpool.New(ctx, bootDSN)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(boot.Close)
	if err := schema.Bootstrap(ctx, boot); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	c = metaCheck(ctx, boot, nil, schema.MigrationStepNames())
	if c.Status != StatusPass || !strings.Contains(c.Detail, "reachable") {
		t.Fatalf("bootstrapped meta = %+v, want pass", c)
	}
	c = metaCheck(ctx, boot, nil, append(schema.MigrationStepNames(), "ddlFromTheFuture"))
	if c.Status != StatusWarn || !strings.Contains(c.Detail, "ddlFromTheFuture") {
		t.Fatalf("meta missing a step = %+v, want warn naming it", c)
	}
}

func hypopgAvailable(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var ok bool
	mustScan(t, pool, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_available_extensions "+
		"WHERE name = 'hypopg')", &ok)
	return ok
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func mustScan(t *testing.T, pool *pgxpool.Pool, sql string, dest ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// createLoginRole creates a throwaway LOGIN role (dropped at cleanup) and
// returns its name and a DSN that logs in as it.
func createLoginRole(t *testing.T, admin *pgxpool.Pool, dsn string) (string, string) {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	role := "doctor_r_" + hex.EncodeToString(b)
	mustExec(t, admin, "CREATE ROLE "+role+" LOGIN PASSWORD 'doctor-pw'")
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = admin.Exec(ctx, "DROP OWNED BY "+role)
		_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+role)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(role, "doctor-pw")
	return role, u.String()
}
