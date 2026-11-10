package fleet

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// capsDB is a throwaway database on the test server, so the extension and
// role changes these tests make never reach other packages' tests.
type capsDB struct {
	admin *pgxpool.Pool // superuser pool on the throwaway database
	dsn   string        // DSN of the throwaway database (superuser)
	major int
}

func newCapsDB(t *testing.T) capsDB {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := t.Context()
	server, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test server: %v", err)
	}
	t.Cleanup(server.Close)
	name := fmt.Sprintf("sage_caps_%d", time.Now().UnixNano())
	if _, err := server.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = server.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	db := capsDB{dsn: withDatabase(t, dsn, name, "", "")}
	if db.admin, err = pgxpool.New(ctx, db.dsn); err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(db.admin.Close)
	if err := db.admin.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::int / 10000").Scan(&db.major); err != nil {
		t.Fatalf("read server version: %v", err)
	}
	return db
}

func withDatabase(t *testing.T, dsn, database, user, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse SAGE_TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + database
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String()
}

// role creates a login role (dropped at cleanup) and returns a pool that
// connects as it, after running grants as the superuser.
func (db capsDB) role(t *testing.T, name string, grants ...string) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	ident := pgx.Identifier{name}.Sanitize()
	db.exec(t, "DROP ROLE IF EXISTS "+ident)
	db.exec(t, "CREATE ROLE "+ident+" LOGIN PASSWORD 'caps-test'")
	t.Cleanup(func() {
		_, _ = db.admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+ident)
	})
	for _, granted := range grants {
		db.exec(t, "GRANT "+pgx.Identifier{granted}.Sanitize()+" TO "+ident)
	}
	pool, err := pgxpool.New(ctx, withDatabase(t, db.dsn, db.database(t), name, "caps-test"))
	if err != nil {
		t.Fatalf("connect as %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func (db capsDB) database(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(db.dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	return u.Path[1:]
}

func (db capsDB) exec(t *testing.T, sql string) {
	t.Helper()
	if _, err := db.admin.Exec(t.Context(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// executeAnalyze is a standing gate that would run every family.
func executeAnalyze(
	_ context.Context, contracts []executor.ActionContract, _ bool,
) []executor.ActionPolicyDecision {
	out := make([]executor.ActionPolicyDecision, len(contracts))
	for i := range contracts {
		out[i] = executor.ActionPolicyDecision{Decision: executor.PolicyDecisionExecute}
	}
	return out
}

func TestMaintainPrivilegeSuperuserIsOK(t *testing.T) {
	db := newCapsDB(t)
	caps := CollectProviderCapabilities(t.Context(), db.admin, "postgres", executeAnalyze)
	got := caps.Permissions["analyze"]
	if got.Status != "ok" || got.Reason == "" {
		t.Fatalf("superuser analyze permission = %#v, want ok with a reason", got)
	}
	if slices.Contains(caps.Blockers, "ANALYZE permission unknown") {
		t.Fatalf("superuser still blocked on ANALYZE: %v", caps.Blockers)
	}
}

// A plain role cannot be proven to own every table, so ANALYZE is
// "limited" (checked at execution), which is informational, not a blocker.
func TestMaintainPrivilegePlainRoleIsLimitedAndReady(t *testing.T) {
	db := newCapsDB(t)
	db.exec(t, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	plain := db.role(t, "sage_caps_plain")
	caps := CollectProviderCapabilities(t.Context(), plain, "postgres", executeAnalyze)
	if got := caps.Permissions["analyze"]; got.Status != "limited" || got.Reason == "" {
		t.Fatalf("plain role analyze permission = %#v, want limited", got)
	}
	if caps.Extensions["pg_stat_statements"] != "available" {
		t.Fatalf("pg_stat_statements = %q", caps.Extensions["pg_stat_statements"])
	}
	if len(caps.Blockers) != 0 || !caps.ReadyForAutoSafe {
		t.Fatalf("plain role readiness: ready=%v blockers=%v", caps.ReadyForAutoSafe,
			caps.Blockers)
	}
	summary := SummarizeReadiness([]DatabaseStatus{{Name: "plain",
		Status: &InstanceStatus{Capabilities: caps}}})
	if summary.ReadyForAutoSafe != 1 || summary.Unknown != 0 {
		t.Fatalf("summary = %+v, want one ready database", summary)
	}
}

// Membership in a managed admin role (here a stand-in named like RDS's)
// proves maintenance rights without superuser.
func TestMaintainPrivilegeManagedAdminRoleIsOK(t *testing.T) {
	db := newCapsDB(t)
	db.exec(t, "DROP ROLE IF EXISTS rds_superuser")
	db.exec(t, "CREATE ROLE rds_superuser NOLOGIN")
	t.Cleanup(func() {
		_, _ = db.admin.Exec(context.Background(), "DROP ROLE IF EXISTS rds_superuser")
	})
	member := db.role(t, "sage_caps_rds", "rds_superuser")
	caps := CollectProviderCapabilities(t.Context(), member, "rds", executeAnalyze)
	if got := caps.Permissions["analyze"]; got.Status != "ok" {
		t.Fatalf("rds_superuser member analyze permission = %#v, want ok", got)
	}
}

// pg_maintain exists from PostgreSQL 17. Listing it on older servers must
// not turn the probe into an error (it would leave the permission unknown).
func TestMaintainPrivilegePgMaintainAcrossVersions(t *testing.T) {
	db := newCapsDB(t)
	if db.major >= 17 {
		member := db.role(t, "sage_caps_maintain", "pg_maintain")
		caps := CollectProviderCapabilities(t.Context(), member, "postgres", executeAnalyze)
		if got := caps.Permissions["analyze"]; got.Status != "ok" {
			t.Fatalf("PG%d pg_maintain member analyze = %#v, want ok", db.major, got)
		}
		return
	}
	var exists bool
	if err := db.admin.QueryRow(t.Context(),
		"SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pg_maintain')").
		Scan(&exists); err != nil || exists {
		t.Fatalf("PG%d pg_maintain exists=%v err=%v; boundary not exercised", db.major,
			exists, err)
	}
	plain := db.role(t, "sage_caps_old")
	caps := CollectProviderCapabilities(t.Context(), plain, "postgres", executeAnalyze)
	if got := caps.Permissions["analyze"]; got.Status != "limited" {
		t.Fatalf("PG%d probe with pg_maintain listed = %#v, want limited", db.major, got)
	}
	for _, limitation := range caps.Limitations {
		if limitation == maintainProbeFailed {
			t.Fatalf("PG%d maintenance probe failed: %v", db.major, caps.Limitations)
		}
	}
}

// CREATE EXTENSION after start reaches readiness on the next refresh.
func TestRefreshPicksUpExtensionInstalledAfterStart(t *testing.T) {
	db := newCapsDB(t)
	db.exec(t, "DROP EXTENSION IF EXISTS pg_stat_statements")
	inst := &DatabaseInstance{Name: "late", Pool: db.admin, Status: &InstanceStatus{
		Capabilities: CollectProviderCapabilities(t.Context(), db.admin, "postgres",
			executeAnalyze)}}
	before := inst.SnapshotStatus().Capabilities
	if before.Extensions["pg_stat_statements"] != "not_installed" ||
		!slices.Contains(before.Blockers, "pg_stat_statements not_installed") {
		t.Fatalf("before: pg_stat_statements=%q blockers=%v",
			before.Extensions["pg_stat_statements"], before.Blockers)
	}
	db.exec(t, "CREATE EXTENSION pg_stat_statements")
	RefreshProviderCapabilities(t.Context(), inst, "postgres", executeAnalyze)
	after := inst.SnapshotStatus().Capabilities
	if after.Extensions["pg_stat_statements"] != "available" {
		t.Fatalf("after refresh pg_stat_statements = %q", after.Extensions["pg_stat_statements"])
	}
	if slices.Contains(after.Blockers, "pg_stat_statements not_installed") {
		t.Fatalf("blocker survived the refresh: %v", after.Blockers)
	}
	if !after.ReadyForAutoSafe {
		t.Fatalf("superuser with pg_stat_statements not ready: %v", after.Blockers)
	}
}

// A NOINHERIT member of an admin role holds none of its privileges unless it
// runs SET ROLE, which pg_sage never does, so ANALYZE stays "limited".
func TestMaintainPrivilegeNoInheritMemberIsLimited(t *testing.T) {
	db := newCapsDB(t)
	db.exec(t, "DROP ROLE IF EXISTS rds_superuser")
	db.exec(t, "CREATE ROLE rds_superuser NOLOGIN")
	t.Cleanup(func() {
		_, _ = db.admin.Exec(context.Background(), "DROP ROLE IF EXISTS rds_superuser")
	})
	// From PG 16 a grant takes its INHERIT option from the member at GRANT
	// time, so NOINHERIT is set first.
	member := db.role(t, "sage_caps_noinherit")
	db.exec(t, "ALTER ROLE sage_caps_noinherit NOINHERIT")
	db.exec(t, "GRANT rds_superuser TO sage_caps_noinherit")
	caps := CollectProviderCapabilities(t.Context(), member, "rds", executeAnalyze)
	if got := caps.Permissions["analyze"]; got.Status != "limited" {
		t.Fatalf("NOINHERIT rds_superuser member analyze = %#v, want limited", got)
	}
}
