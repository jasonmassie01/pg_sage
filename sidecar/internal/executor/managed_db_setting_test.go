package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

var managedEnvironments = []string{"rds", "aurora", "cloud-sql", "alloydb", "azure",
	"neon", "supabase", "lakebase"}

func managedExecutor(pool *pgxpool.Pool, env string) *Executor {
	cfg := config.DefaultConfig()
	cfg.CloudEnvironment = env
	return New(pool, cfg, time.Time{}, nopLog)
}

// unreachablePool connects lazily to a port nothing listens on, so every
// query fails with a connection error.
func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(
		"postgres://nobody:none@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestManagedDatabaseSettingContextReadErrorFailsClosed(t *testing.T) {
	pool := unreachablePool(t)
	for _, env := range managedEnvironments {
		err := managedExecutor(pool, env).checkManagedDatabaseSetting(context.Background(),
			"ALTER DATABASE app SET work_mem = '16MB'")
		if !errors.Is(err, ErrManagedConfigUnsupported) {
			t.Fatalf("%s: context read error = %v, want ErrManagedConfigUnsupported", env, err)
		}
		if !strings.Contains(err.Error(), "work_mem") {
			t.Fatalf("%s: error does not name the setting: %v", env, err)
		}
	}
}

func TestManagedDatabaseSettingNilPoolAndUnparsableFailClosed(t *testing.T) {
	for _, env := range managedEnvironments {
		e := managedExecutor(nil, env)
		for _, sql := range []string{"ALTER DATABASE app SET work_mem = '16MB'",
			"ALTER DATABASE app OWNER TO someone", "ALTER DATABASE"} {
			if err := e.checkManagedDatabaseSetting(context.Background(), sql); !errors.Is(
				err, ErrManagedConfigUnsupported) {
				t.Fatalf("%s %q: err = %v, want ErrManagedConfigUnsupported", env, sql, err)
			}
		}
	}
}

func TestManagedDatabaseSettingIgnoresSelfManagedAndOtherSQL(t *testing.T) {
	for _, env := range []string{"", "self-managed", "on-prem"} {
		err := managedExecutor(nil, env).checkManagedDatabaseSetting(context.Background(),
			"ALTER DATABASE app SET max_wal_size = '4GB'")
		if err != nil {
			t.Fatalf("%q: self-managed refused: %v", env, err)
		}
	}
	for _, sql := range []string{"ALTER SYSTEM SET work_mem = '16MB'", "VACUUM public.t",
		"CREATE INDEX CONCURRENTLY i ON t (a)"} {
		if err := managedExecutor(nil, "cloud-sql").checkManagedDatabaseSetting(
			context.Background(), sql); err != nil {
			t.Fatalf("%q is not a database setting but was refused: %v", sql, err)
		}
	}
}

// CHECK-16: a stale ALTER DATABASE ... SET checkpoint_timeout on Cloud SQL
// is refused before it reaches PostgreSQL; work_mem still passes.
func TestManagedDatabaseSettingRefusesInstanceSettings(t *testing.T) {
	pool, ctx := requireDB(t)
	for _, env := range managedEnvironments {
		e := managedExecutor(pool, env)
		for _, sql := range []string{"ALTER DATABASE x SET checkpoint_timeout = '15min'",
			"ALTER DATABASE x SET max_wal_size = '4GB'",
			`ALTER DATABASE "x" SET shared_buffers = '1GB'`} {
			err := e.checkGUCValueSafety(ctx, sql)
			if !errors.Is(err, ErrManagedConfigUnsupported) ||
				!strings.Contains(err.Error(), "context") {
				t.Fatalf("%s %q: err = %v, want a context refusal", env, sql, err)
			}
		}
		if err := e.checkGUCValueSafety(ctx, "ALTER DATABASE x SET work_mem = '16MB'"); err != nil {
			t.Fatalf("%s: user-context work_mem refused: %v", env, err)
		}
	}
	var database string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	_, err := managedExecutor(pool, "cloud-sql").prepareConfigChange(ctx,
		"ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+
			" SET checkpoint_timeout = '15min'")
	if !errors.Is(err, ErrManagedConfigUnsupported) {
		t.Fatalf("prepareConfigChange on cloud-sql = %v, want ErrManagedConfigUnsupported", err)
	}
}

// A superuser-context setting passes only for a role that may SET it:
// a superuser, or on PostgreSQL 15+ a role granted SET on the parameter.
func TestManagedDatabaseSettingSuperuserContextNeedsPrivilege(t *testing.T) {
	pool, ctx := requireDB(t)
	sql := "ALTER DATABASE x SET log_min_duration_statement = '1s'"
	if err := managedExecutor(pool, "rds").checkManagedDatabaseSetting(ctx, sql); err != nil {
		t.Fatalf("superuser refused a superuser-context setting: %v", err)
	}
	role := "sage_guc_probe_role"
	mustExec(t, pool, ctx, "DROP ROLE IF EXISTS "+role)
	mustExec(t, pool, ctx, "CREATE ROLE "+role+" NOLOGIN")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP OWNED BY "+role)
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	cfg := pool.Config()
	cfg.MaxConns = 1
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+role)
		return err
	}
	rolePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rolePool.Close)
	e := managedExecutor(rolePool, "rds")
	if err := e.checkManagedDatabaseSetting(ctx, sql); !errors.Is(err,
		ErrManagedConfigUnsupported) {
		t.Fatalf("unprivileged role passed a superuser-context setting: %v", err)
	}
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 150000 {
		return // PostgreSQL 14 has no parameter privileges: superuser or nothing
	}
	mustExec(t, pool, ctx, "GRANT SET ON PARAMETER log_min_duration_statement TO "+role)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"REVOKE SET ON PARAMETER log_min_duration_statement FROM "+role)
	})
	if err := e.checkManagedDatabaseSetting(ctx, sql); err != nil {
		t.Fatalf("role granted SET was refused: %v", err)
	}
}

// State transition: on Neon a user-context change with no caller rollback
// is prepared for execution and records the prior value it captured.
func TestManagedDatabaseSettingNeonRecordsCapturedPrior(t *testing.T) {
	pool, ctx := requireDB(t)
	var database string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{database}.Sanitize()
	mustExec(t, pool, ctx, "ALTER DATABASE "+quoted+" SET work_mem = '9MB'")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "ALTER DATABASE "+quoted+" RESET work_mem")
	})
	change, err := managedExecutor(pool, "neon").prepareConfigChange(ctx,
		"ALTER DATABASE "+quoted+" SET work_mem = '16MB'")
	if err != nil || change == nil {
		t.Fatalf("Neon user-context change refused: change=%v err=%v", change, err)
	}
	state := map[string]any{}
	change.record(state)
	recorded, ok := state["config_change"].(map[string]any)
	if !ok {
		t.Fatalf("no config change recorded: %v", state)
	}
	want := "ALTER DATABASE " + quoted + " SET work_mem = '9MB'"
	if recorded["rollback_sql"] != want || recorded["name"] != "work_mem" {
		t.Fatalf("recorded %v, want rollback %q", recorded, want)
	}
	prior, ok := recorded["prior"].(databaseSettingPrior)
	if !ok || prior.Value == nil || *prior.Value != "9MB" {
		t.Fatalf("captured prior = %#v", recorded["prior"])
	}
}
