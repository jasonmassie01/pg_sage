//go:build integration

package advisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// advisors-10: every documented GUC range edge, for every provider
// profile, goes through GateConfigFindingsHost with the live pg_settings
// rows of the role that would run it, and every statement the gate lets
// through runs on this server. The expected outcome is PostgreSQL's rule,
// not the router's: ALTER SYSTEM takes any documented setting from a
// superuser; ALTER DATABASE takes only user-context settings, or
// superuser-context ones the role may SET. A change the gate withholds is
// also shown to be one PostgreSQL refuses, so over-refusal fails too.
// No case calls pg_reload_conf(): edge values never take effect. Only the
// cleanup reloads, after it has reset every documented setting.

// No concurrent access tests: the matrix holds the cluster-wide
// alter_system lock, so it runs alone by design.

func TestConfigSQLMatrix(t *testing.T) {
	env := newMatrixEnv(t)
	edges := rangeEdges()
	for _, provider := range matrixProviders {
		t.Run(provider, func(t *testing.T) {
			for _, e := range edges {
				t.Run(e.guc+"/"+e.edge+"/"+e.form, func(t *testing.T) {
					env.runEdge(t, provider, e)
				})
			}
		})
	}
}

func (env *matrixEnv) runEdge(t *testing.T, provider string, e rangeEdge) {
	set := fmt.Sprintf("ALTER SYSTEM SET %s = '%s'", e.guc, e.value)
	got := env.gate(t, provider, set)
	want := env.expectExecutable(t, provider, e.guc)
	if (got.RecommendedSQL != "") != want {
		t.Fatalf("%s on %s (context %q): executable=%v, want %v; SQL %q (%s); message %q",
			e.guc, provider, env.context(t, provider, e.guc), got.RecommendedSQL != "",
			want, got.RecommendedSQL, env.tryExec(provider, got.RecommendedSQL),
			got.Recommendation)
	}
	reset := env.gate(t, provider, "ALTER SYSTEM RESET "+e.guc)
	if !want {
		assertRefused(t, provider, e.guc, got)
		if reset.RecommendedSQL != "" {
			t.Errorf("%s on %s: SET withheld but RESET runs: %q", e.guc, provider,
				reset.RecommendedSQL)
		}
		env.assertPostgresRefuses(t, provider, e)
		return
	}
	if provider == "self-managed" && !pgconf.DatabaseScopeApprovalOnly(e.guc) {
		env.applySystem(t, e, got, reset)
		return
	}
	env.applyDatabase(t, provider, e, got, reset)
}

// expectExecutable is PostgreSQL's rule for the role the profile uses.
func (env *matrixEnv) expectExecutable(t *testing.T, provider, guc string) bool {
	if provider == "self-managed" {
		return true
	}
	row := env.row(t, provider, guc)
	return row.Context == "user" || (row.Context == "superuser" && row.SetPrivilege)
}

func (env *matrixEnv) gate(t *testing.T, provider, sql string) analyzer.Finding {
	t.Helper()
	if provider == "self-managed" {
		// A database-scope setting (advisors-07) runs as ALTER DATABASE on
		// the scratch database even on a self-managed server.
		db := ""
		if stmt, ok := pgconf.ParseAlterSystem(sql); ok &&
			pgconf.DatabaseScopeApprovalOnly(stmt.Name) {
			db = env.scratch
		}
		return gateMatrix(t, matrixFinding(sql), provider, db, env.adminRows)
	}
	return gateMatrix(t, matrixFinding(sql), provider, env.scratch, env.tenantRows)
}

// applySystem runs the self-managed SET and RESET as the admin and reads
// postgresql.auto.conf back through pg_file_settings.
func (env *matrixEnv) applySystem(t *testing.T, e rangeEdge, set, reset analyzer.Finding) {
	if !strings.HasPrefix(set.RecommendedSQL, "ALTER SYSTEM SET ") ||
		reset.RecommendedSQL != "ALTER SYSTEM RESET "+e.guc {
		t.Fatalf("%s on self-managed: SQL %q / reset %q", e.guc, set.RecommendedSQL,
			reset.RecommendedSQL)
	}
	matrixExec(t, env.admin, "self-managed", e.guc, set.RecommendedSQL)
	if got := env.autoConf(t, e.guc); got != e.value {
		t.Errorf("%s: postgresql.auto.conf has %q, want %q", e.guc, got, e.value)
	}
	matrixExec(t, env.admin, "self-managed", e.guc, reset.RecommendedSQL)
	if got := env.autoConf(t, e.guc); got != "" {
		t.Errorf("%s: still in postgresql.auto.conf after RESET: %q", e.guc, got)
	}
}

// applyDatabase runs the managed SET, its rollback hint and the gated
// RESET as the scratch database's non-superuser owner.
func (env *matrixEnv) applyDatabase(t *testing.T, provider string, e rangeEdge,
	set, reset analyzer.Finding) {
	prefix := "ALTER DATABASE " + sanitize.QuoteIdentifier(env.scratch)
	wantReset := prefix + " RESET " + e.guc
	if !strings.HasPrefix(set.RecommendedSQL, prefix+" SET ") ||
		set.RollbackSQL != wantReset || reset.RecommendedSQL != wantReset {
		t.Fatalf("%s on %s: SQL %q, rollback %q, reset %q", e.guc, provider,
			set.RecommendedSQL, set.RollbackSQL, reset.RecommendedSQL)
	}
	matrixExec(t, env.tenant, provider, e.guc, set.RecommendedSQL)
	if got := env.databaseSetting(t, e.guc); got != e.guc+"="+e.value {
		t.Errorf("%s on %s: database setting %q, want %s=%s", e.guc, provider, got,
			e.guc, e.value)
	}
	matrixExec(t, env.tenant, provider, e.guc, set.RollbackSQL)
	if got := env.databaseSetting(t, e.guc); got != "" {
		t.Errorf("%s on %s: rollback left %q", e.guc, provider, got)
	}
	matrixExec(t, env.tenant, provider, e.guc, reset.RecommendedSQL)
}

// assertPostgresRefuses shows a withheld managed change is one PostgreSQL
// itself refuses as ALTER DATABASE for the scratch database's owner.
func (env *matrixEnv) assertPostgresRefuses(t *testing.T, provider string, e rangeEdge) {
	if provider == "self-managed" {
		return
	}
	db := sanitize.QuoteIdentifier(env.scratch)
	sql := fmt.Sprintf("ALTER DATABASE %s SET %s = '%s'", db, e.guc, e.value)
	_, err := env.tenant.Exec(context.Background(), sql)
	if err == nil {
		matrixExec(t, env.tenant, provider, e.guc, "ALTER DATABASE "+db+" RESET "+e.guc)
		t.Errorf("%s on %s: the gate withheld it, but PostgreSQL accepts %q", e.guc,
			provider, sql)
		return
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != "55P02" && pgErr.Code != "42501") {
		t.Errorf("%s on %s: %q refused for an unexpected reason: %v", e.guc, provider,
			sql, err)
	}
}

// tryExec reports what PostgreSQL makes of a statement the matrix did not
// expect to run. One it accepts is left for the environment's cleanup:
// ALTER SYSTEM RESET of every documented GUC and the scratch database's
// drop.
func (env *matrixEnv) tryExec(provider, sql string) string {
	conn := env.tenant
	if provider == "self-managed" {
		conn = env.admin
	}
	if sql == "" {
		return "no SQL"
	}
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		return "PostgreSQL refuses it: " + describePGError(err)
	}
	return "PostgreSQL accepts it"
}

// matrixExec runs one gated statement; a failure names the setting, the
// provider, the statement and the SQLSTATE.
func matrixExec(t testing.TB, conn *pgx.Conn, provider, guc, sql string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s on %s: %q failed: %s", guc, provider, sql, describePGError(err))
	}
}

func describePGError(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("SQLSTATE %s: %s", pgErr.Code, pgErr.Message)
	}
	return err.Error()
}
