//go:build integration

package advisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// matrixEnv is the config SQL matrix's server state. Self-managed runs as
// the admin; managed profiles run as a NOSUPERUSER, NOCREATEDB role that
// owns a scratch database, as a tenant on a managed service would.
type matrixEnv struct {
	admin, tenant         *pgx.Conn
	scratch               string
	adminRows, tenantRows []collector.PGSetting
}

func newMatrixEnv(t *testing.T) *matrixEnv {
	t.Helper()
	ctx := context.Background()
	base := testdb.SkipUnlessLive(t)
	// ALTER SYSTEM is cluster-wide: hold the lock the executor's
	// ALTER SYSTEM round trips hold.
	release, err := testdb.LockCluster(ctx, base, "alter_system")
	if err != nil {
		t.Fatalf("config matrix: %v", err)
	}
	t.Cleanup(release)
	admin := matrixConnect(t, os.Getenv("SAGE_DATABASE_URL"), "", "")
	t.Cleanup(func() { resetSystemSettings(t, admin) })
	role, password := matrixRole(t, admin)
	dsn := testdb.CreateDatabase(t, "cfgmatrix")
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("config matrix: parse scratch DSN: %v", err)
	}
	matrixExec(t, admin, "setup", "owner", "ALTER DATABASE "+
		sanitize.QuoteIdentifier(cfg.Database)+" OWNER TO "+sanitize.QuoteIdentifier(role))
	tenant := matrixConnect(t, dsn, role, password)
	return &matrixEnv{admin: admin, tenant: tenant, scratch: cfg.Database,
		adminRows: matrixSettings(t, admin), tenantRows: matrixSettings(t, tenant)}
}

// matrixConnect opens one connection, as user when one is given.
func matrixConnect(t *testing.T, dsn, user, password string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("config matrix: parse DSN: %v", err)
	}
	if user != "" {
		cfg.User, cfg.Password = user, password
	}
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("config matrix: connect as %q: %v", cfg.User, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// matrixRole creates the tenant role; its cleanup runs after the scratch
// database (created later) is dropped.
func matrixRole(t testing.TB, admin *pgx.Conn) (role, password string) {
	t.Helper()
	role, password = "pgsage_matrix_"+randomHex(t, 4), randomHex(t, 16)
	// DDL takes no bind parameters; both values are generated hex, quoted.
	// The statement carries the password, so a failure names only the role.
	_, err := admin.Exec(context.Background(), "CREATE ROLE "+sanitize.QuoteIdentifier(role)+
		" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD "+sanitize.QuoteLiteral(password))
	if err != nil {
		t.Fatalf("config matrix: create role %s: %s", role, describePGError(err))
	}
	t.Cleanup(func() {
		_, err := admin.Exec(context.Background(),
			"DROP ROLE IF EXISTS "+sanitize.QuoteIdentifier(role))
		if err != nil {
			t.Errorf("config matrix: drop role %s: %s", role, describePGError(err))
		}
	})
	return role, password
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("config matrix: random: %v", err)
	}
	return hex.EncodeToString(b)
}

// resetSystemSettings leaves postgresql.auto.conf without any documented
// GUC, even when a case stopped between its SET and RESET. It then reloads,
// so a value another suite's reload put in effect meanwhile is undone too.
func resetSystemSettings(t *testing.T, admin *pgx.Conn) {
	for _, name := range documentedGUCs() {
		if _, err := admin.Exec(context.Background(), "ALTER SYSTEM RESET "+name); err != nil {
			t.Errorf("config matrix cleanup: reset %s: %s", name, describePGError(err))
		}
	}
	if _, err := admin.Exec(context.Background(), "SELECT pg_reload_conf()"); err != nil {
		t.Errorf("config matrix cleanup: reload after reset: %s", describePGError(err))
	}
}

// matrixSettings reads the documented GUCs' pg_settings rows as the
// connected role, marking SetPrivilege as the collector does.
func matrixSettings(t *testing.T, conn *pgx.Conn) []collector.PGSetting {
	t.Helper()
	ctx := context.Background()
	rows, err := conn.Query(ctx, `SELECT name, setting, COALESCE(unit, ''), source,
		COALESCE(pending_restart, false), context
		FROM pg_settings WHERE name = ANY($1) ORDER BY name`, documentedGUCs())
	if err != nil {
		t.Fatalf("config matrix: read pg_settings: %v", err)
	}
	settings, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (collector.PGSetting,
		error) {
		var s collector.PGSetting
		err := r.Scan(&s.Name, &s.Setting, &s.Unit, &s.Source, &s.PendingRestart, &s.Context)
		return s, err
	})
	if err != nil {
		t.Fatalf("config matrix: scan pg_settings: %v", err)
	}
	if len(settings) != len(pgconf.Docs) {
		t.Fatalf("config matrix: pg_settings has %d of %d documented GUCs",
			len(settings), len(pgconf.Docs))
	}
	markMatrixPrivileges(t, conn, settings)
	return settings
}

// markMatrixPrivileges: a superuser may SET every superuser-context
// setting; on PG15+ another role may SET the ones it was granted.
func markMatrixPrivileges(t *testing.T, conn *pgx.Conn, settings []collector.PGSetting) {
	t.Helper()
	ctx := context.Background()
	var super bool
	var version int
	if err := conn.QueryRow(ctx, `SELECT rolsuper,
		current_setting('server_version_num')::int FROM pg_roles
		WHERE rolname = current_user`).Scan(&super, &version); err != nil {
		t.Fatalf("config matrix: read role: %v", err)
	}
	for i := range settings {
		if settings[i].Context != "superuser" {
			continue
		}
		settings[i].SetPrivilege = super
		if !super && version >= 150000 {
			if err := conn.QueryRow(ctx, "SELECT has_parameter_privilege($1, 'SET')",
				settings[i].Name).Scan(&settings[i].SetPrivilege); err != nil {
				t.Fatalf("config matrix: SET privilege on %s: %v", settings[i].Name, err)
			}
		}
	}
}

func (env *matrixEnv) row(t *testing.T, provider, guc string) collector.PGSetting {
	t.Helper()
	rows := env.tenantRows
	if provider == "self-managed" {
		rows = env.adminRows
	}
	for _, s := range rows {
		if s.Name == guc {
			return s
		}
	}
	t.Fatalf("config matrix: no pg_settings row for %s", guc)
	return collector.PGSetting{}
}

func (env *matrixEnv) context(t *testing.T, provider, guc string) string {
	return env.row(t, provider, guc).Context
}

// autoConf is the value postgresql.auto.conf holds for guc ("" if none).
func (env *matrixEnv) autoConf(t *testing.T, guc string) string {
	t.Helper()
	var value string
	err := env.admin.QueryRow(context.Background(), `SELECT setting FROM pg_file_settings
		WHERE name = $1 AND sourcefile LIKE '%postgresql.auto.conf'
		ORDER BY seqno DESC LIMIT 1`, guc).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("config matrix: read pg_file_settings for %s: %v", guc, err)
	}
	return value
}

// databaseSetting is the scratch database's own entry for guc ("" if none).
func (env *matrixEnv) databaseSetting(t *testing.T, guc string) string {
	t.Helper()
	var value string
	err := env.admin.QueryRow(context.Background(), `SELECT cfg
		FROM pg_db_role_setting s JOIN pg_database d ON d.oid = s.setdatabase,
		     unnest(s.setconfig) AS cfg
		WHERE d.datname = $1 AND s.setrole = 0 AND split_part(cfg, '=', 1) = $2`,
		env.scratch, guc).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("config matrix: read database setting %s: %v", guc, err)
	}
	return value
}
