package collector

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/pgconf"
)

// failingQuerier fails every query and counts the calls.
type failingQuerier struct{ calls int }

var errQuerierDown = errors.New("connection refused")

func (q *failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.calls++
	return nil, errQuerierDown
}

func (q *failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	q.calls++
	return failedRow{}
}

type failedRow struct{}

func (failedRow) Scan(...any) error { return errQuerierDown }

func superuserRows() []PGSetting {
	return []PGSetting{{Name: "work_mem", Context: "user"},
		{Name: "log_min_duration_statement", Context: "superuser"},
		{Name: "max_wal_size", Context: "sighup"}}
}

func TestMarkSetPrivilegesSkipsQueryWithoutSuperuserRows(t *testing.T) {
	q := &failingQuerier{}
	for _, settings := range [][]PGSetting{nil, {}, {{Name: "work_mem", Context: "user"}}} {
		if err := markSetPrivileges(context.Background(), q, settings); err != nil {
			t.Fatalf("no superuser-context rows still failed: %v", err)
		}
	}
	if q.calls != 0 {
		t.Fatalf("queried the server %d times with nothing to check", q.calls)
	}
}

func TestMarkSetPrivilegesErrorLeavesRowsUnprivileged(t *testing.T) {
	settings := superuserRows()
	err := markSetPrivileges(context.Background(), &failingQuerier{}, settings)
	if !errors.Is(err, errQuerierDown) || !strings.Contains(err.Error(), "SET privilege") {
		t.Fatalf("error = %v, want a wrapped SET privilege error", err)
	}
	for _, s := range settings {
		if s.SetPrivilege {
			t.Fatalf("%s marked settable after a failed read", s.Name)
		}
	}
}

func TestMarkSetPrivilegesSuperuserAndGrantedRole(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	settings := superuserRows()
	if err := markSetPrivileges(ctx, pool, settings); err != nil {
		t.Fatal(err)
	}
	if !settings[1].SetPrivilege || settings[0].SetPrivilege || settings[2].SetPrivilege {
		t.Fatalf("superuser marks = %+v, want only the superuser-context row", settings)
	}
	role := "sage_collector_set_probe"
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	rolePool := rolePoolFor(t, pool, role)
	settings = superuserRows()
	if err := markSetPrivileges(ctx, rolePool, settings); err != nil {
		t.Fatal(err)
	}
	if settings[1].SetPrivilege {
		t.Fatal("unprivileged role marked able to SET a superuser-context setting")
	}
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 150000 {
		return // PostgreSQL 14 has no parameter privileges
	}
	exec("GRANT SET ON PARAMETER log_min_duration_statement TO " + role)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"REVOKE SET ON PARAMETER log_min_duration_statement FROM "+role)
	})
	settings = superuserRows()
	if err := markSetPrivileges(ctx, rolePool, settings); err != nil {
		t.Fatal(err)
	}
	if !settings[1].SetPrivilege {
		t.Fatal("role granted SET on the parameter not marked settable")
	}
}

// rolePoolFor creates a NOLOGIN role and a pool whose sessions SET ROLE to
// it, so current_user is an unprivileged role on the same server.
func rolePoolFor(t *testing.T, pool *pgxpool.Pool, role string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP ROLE "+role) })
	cfg := pool.Config()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+role)
		return err
	}
	rolePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rolePool.Close)
	return rolePool
}

// Every GUC the advisor may change must be in the config snapshot, or its
// pg_settings context is unknown and the managed rewrite fails closed.
func TestConfigSettingsQueryCoversEveryDocumentedGUC(t *testing.T) {
	listed := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(configSettingsSQL, -1)
	names := map[string]bool{}
	for _, m := range listed {
		names[m[1]] = true
	}
	for name := range pgconf.Docs {
		if !names[name] && !strings.HasPrefix(name, "autovacuum") {
			t.Errorf("config snapshot does not collect documented GUC %s", name)
		}
	}
	if !strings.Contains(configSettingsSQL, "name LIKE 'autovacuum%'") {
		t.Fatal("config snapshot lost the autovacuum settings")
	}
}
