package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// vacuum-06 integration tests on real PostgreSQL (14 and 17 legs): the
// precheck's verdict for each role matches what the server then does, and
// a VACUUM the server skipped is recorded as failed.

// No concurrent access tests: the precheck is one read before each action,
// under the action's lease.

func vpServerVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("read server_version_num: %v", err)
	}
	return version
}

// vpTable creates public.<name> owned by the admin with 2000 rows; autovacuum
// is off so only the test's own VACUUM moves its statistics.
func vpTable(t *testing.T, ctx context.Context, admin *pgxpool.Pool, name string) string {
	t.Helper()
	if _, err := admin.Exec(ctx, "DROP TABLE IF EXISTS public."+name+"; CREATE TABLE public."+
		name+" WITH (autovacuum_enabled = false) AS SELECT g AS a FROM generate_series(1, 2000) g",
	); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP TABLE IF EXISTS public."+name)
	})
	return "public." + name
}

func vpRoleName(label string) string {
	return fmt.Sprintf("vp_%s_%06x", label, time.Now().UnixNano()&0xffffff)
}

// vpGrantSage lets role run the executor's own bookkeeping in schema sage,
// as the sidecar's role does; it gains nothing on user tables.
func vpGrantSage(t *testing.T, ctx context.Context, admin *pgxpool.Pool, role string) {
	t.Helper()
	rq := pgx.Identifier{role}.Sanitize()
	if _, err := admin.Exec(ctx, "GRANT USAGE ON SCHEMA sage TO "+rq+
		"; GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA sage TO "+rq+
		"; GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA sage TO "+rq); err != nil {
		t.Fatalf("grant sage to %s: %v", role, err)
	}
}

// vpNotices runs sql in a session of pool's role and returns its notices.
func vpNotices(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) (
	string, error) {
	t.Helper()
	cfg := pool.Config().ConnConfig.Copy()
	var notices strings.Builder
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		notices.WriteString(n.Severity + ": " + n.Message + "\n")
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(ctx, sql)
	return notices.String(), err
}

func vpVacuumCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(vacuum_count, 0) FROM pg_stat_user_tables
		WHERE relid = to_regclass($1)`, table).Scan(&count); err != nil {
		t.Fatalf("read vacuum_count of %s: %v", table, err)
	}
	return count
}

// vpVacuumAdvances runs VACUUM as pool's role and waits for vacuum_count
// to advance; PostgreSQL 14's collector can drop a report, so it reruns
// the VACUUM once.
func vpVacuumAdvances(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()
	before := vpVacuumCount(t, ctx, pool, table)
	for attempt := 0; attempt < 2; attempt++ {
		if notices, err := vpNotices(t, ctx, pool, "VACUUM "+table); err != nil ||
			strings.Contains(notices, "skipping") {
			t.Fatalf("VACUUM %s: %v, notices %q; want it to run", table, err, notices)
		}
		for i := 0; i < 40; i++ {
			if vpVacuumCount(t, ctx, pool, table) > before {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	t.Fatalf("vacuum_count of %s never advanced past %d", table, before)
}

// vpVacuumSkipped runs VACUUM as pool's role and requires the server to
// skip it with a WARNING while the statement still succeeds.
func vpVacuumSkipped(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) {
	t.Helper()
	before := vpVacuumCount(t, ctx, pool, table)
	notices, err := vpNotices(t, ctx, pool, "VACUUM "+table)
	if err != nil || !strings.Contains(notices, "WARNING") ||
		!strings.Contains(notices, "skipping") {
		t.Fatalf("VACUUM %s: %v, notices %q; want success with a skipping WARNING",
			table, err, notices)
	}
	time.Sleep(time.Second)
	if after := vpVacuumCount(t, ctx, pool, table); after != before {
		t.Fatalf("vacuum_count of %s moved %d -> %d for a skipped VACUUM", table, before, after)
	}
}

func TestMaintenancePrivilegeDB_NonOwnerRefused(t *testing.T) {
	admin, ctx := requireDB(t)
	table := vpTable(t, ctx, admin, "vp_nonowner_t")
	pool, role := grantsRoleNamed(t, testdb.SkipUnlessLive(t), vpRoleName("nonowner"))
	got, err := readMaintenancePrivilege(ctx, pool, table, "vacuum_table")
	if err != nil {
		t.Fatalf("readMaintenancePrivilege: %v", err)
	}
	want := "ownership required"
	if vpServerVersion(t, ctx, admin) >= maintainMinVersion {
		want = "GRANT MAINTAIN ON TABLE " + table + " TO " + pgx.Identifier{role}.Sanitize()
	}
	if got.Allowed || !got.Exists || !strings.Contains(got.Detail, want) {
		t.Fatalf("privilege = %+v, want refused naming %q", got, want)
	}
	vpVacuumSkipped(t, ctx, pool, table)
	alter, err := readMaintenancePrivilege(ctx, pool, table, "set_table_autovacuum")
	if err != nil || alter.Allowed || !strings.Contains(alter.Detail, "ownership required") {
		t.Fatalf("alter privilege = %+v, %v; want refused: ownership required", alter, err)
	}
}

func TestMaintenancePrivilegeDB_DatabaseOwnerAllowed(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "vacuum_precheck")
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	table := vpTable(t, ctx, admin, "vp_dbowner_t")
	pool, role := grantsRoleNamed(t, dsn, vpRoleName("dbowner"))
	u, _ := url.Parse(dsn)
	db := pgx.Identifier{strings.TrimPrefix(u.Path, "/")}.Sanitize()
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+db+" OWNER TO "+
		pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatalf("make %s the database owner: %v", role, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "ALTER DATABASE "+db+" OWNER TO CURRENT_USER")
	})
	got, err := readMaintenancePrivilege(ctx, pool, table, "vacuum_table")
	if err != nil || !got.Allowed || got.Basis != "database_owner" {
		t.Fatalf("privilege = %+v, %v; want allowed as database owner", got, err)
	}
	vpVacuumAdvances(t, ctx, pool, table)
	alter, err := readMaintenancePrivilege(ctx, pool, table, "set_table_autovacuum")
	if err != nil || alter.Allowed {
		t.Fatalf("alter privilege = %+v, %v; want refused for a database owner", alter, err)
	}
	_, err = pool.Exec(ctx, "ALTER TABLE "+table+" SET (autovacuum_vacuum_scale_factor = 0.05)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("ALTER TABLE as database owner = %v, want 42501 as the precheck predicts", err)
	}
}

// PostgreSQL 17+: MAINTAIN on the table or pg_maintain membership allows
// VACUUM, never ALTER TABLE. Below 17 the privilege does not exist, which
// is why the precheck never names it there.
func TestMaintenancePrivilegeDB_MaintainGrants(t *testing.T) {
	admin, ctx := requireDB(t)
	table := vpTable(t, ctx, admin, "vp_maintain_t")
	pool, role := grantsRoleNamed(t, testdb.SkipUnlessLive(t), vpRoleName("maintain"))
	rq := pgx.Identifier{role}.Sanitize()
	_, grantErr := admin.Exec(ctx, "GRANT MAINTAIN ON "+table+" TO "+rq)
	if vpServerVersion(t, ctx, admin) < maintainMinVersion {
		if grantErr == nil || !strings.Contains(grantErr.Error(), "unrecognized privilege") {
			t.Fatalf("GRANT MAINTAIN below 17 = %v, want unrecognized privilege type", grantErr)
		}
		got, err := readMaintenancePrivilege(ctx, pool, table, "vacuum_table")
		if err != nil || got.Allowed {
			t.Fatalf("privilege below 17 = %+v, %v; want refused without an error", got, err)
		}
		return
	}
	if grantErr != nil {
		t.Fatalf("GRANT MAINTAIN: %v", grantErr)
	}
	got, err := readMaintenancePrivilege(ctx, pool, table, "vacuum_table")
	if err != nil || !got.Allowed || got.Basis != "maintain" {
		t.Fatalf("privilege = %+v, %v; want allowed by MAINTAIN", got, err)
	}
	vpVacuumAdvances(t, ctx, pool, table)
	alter, err := readMaintenancePrivilege(ctx, pool, table, "set_table_autovacuum")
	if err != nil || alter.Allowed || !strings.Contains(alter.Detail, "ownership required") {
		t.Fatalf("alter privilege = %+v, %v; want refused although MAINTAIN is held", alter, err)
	}
	_, err = pool.Exec(ctx, "ALTER TABLE "+table+" SET (autovacuum_vacuum_scale_factor = 0.05)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("ALTER TABLE with MAINTAIN = %v, want 42501 as the precheck predicts", err)
	}
	if _, err := admin.Exec(ctx, "REVOKE MAINTAIN ON "+table+" FROM "+rq+
		"; GRANT pg_maintain TO "+rq); err != nil {
		t.Fatalf("swap MAINTAIN for pg_maintain: %v", err)
	}
	got, err = readMaintenancePrivilege(ctx, pool, table, "vacuum_table")
	if err != nil || !got.Allowed || got.Basis != "maintain" {
		t.Fatalf("pg_maintain privilege = %+v, %v; want allowed", got, err)
	}
}

func vpDecision(t *testing.T, ctx context.Context, admin *pgxpool.Pool, table string) int64 {
	t.Helper()
	var id int64
	if err := admin.QueryRow(ctx, `INSERT INTO sage.decision
		(feature, intent, target_objects, verdict, risk_tier, reason, evidence_id)
		VALUES ('vacuum', 'vacuum', $1::jsonb, 'execute', 'safe', 'authorized', $2)
		RETURNING id`, `["`+table+`"]`,
		fmt.Sprintf("vp-%s-%d", t.Name(), time.Now().UnixNano())).Scan(&id); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DELETE FROM sage.decision WHERE id=$1", id)
	})
	return id
}

// vpApply runs sql through Apply as an authorized background change and
// reports whether Execute ran.
func vpApply(ctx context.Context, e *Executor, sql string, decisionID int64) (bool, error) {
	executed := false
	_, err := e.Apply(ctx, ActionIntent{
		Request: policy.ActionRequest{SQL: sql, Feature: "vacuum"},
		FirstDecision: &ActionPolicyDecision{Decision: PolicyDecisionExecute,
			RiskTier: "safe", DecisionID: decisionID},
		Execute: func(context.Context, ActionPolicyDecision) (int64, error) {
			executed = true
			return 0, nil
		},
	})
	return executed, err
}

func vpDecisionRow(t *testing.T, ctx context.Context, admin *pgxpool.Pool, id int64) (
	string, string, string) {
	t.Helper()
	var verdict, reason, detail string
	if err := admin.QueryRow(ctx, `SELECT verdict, reason,
		COALESCE(evidence->'maintenance_privilege'->>'detail', '')
		FROM sage.decision WHERE id=$1`, id).Scan(&verdict, &reason, &detail); err != nil {
		t.Fatalf("read decision %d: %v", id, err)
	}
	return verdict, reason, detail
}

// CHECK-01: a non-owner's VACUUM is withheld with capability_missing and
// writes no action_log row.
func TestApplyWithholdsNonOwnerVacuum(t *testing.T) {
	admin, ctx := requireDB(t)
	table := vpTable(t, ctx, admin, "vp_apply_t")
	pool, role := grantsRoleNamed(t, testdb.SkipUnlessLive(t), vpRoleName("apply"))
	vpGrantSage(t, ctx, admin, role)
	decisionID := vpDecision(t, ctx, admin, table)
	e := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	sql := "VACUUM " + table
	executed, err := vpApply(ctx, e, sql, decisionID)
	var withheld *WithheldError
	if executed || !errors.As(err, &withheld) ||
		withheld.Decision.BlockedReason != reasonCapabilityMissing {
		t.Fatalf("executed=%v err=%v, want withheld with %s", executed, err,
			reasonCapabilityMissing)
	}
	if !errors.Is(err, ErrMaintenancePrivilegeMissing) || ManualExecuteStatus(err) != 409 {
		t.Fatalf("err = %v, want ErrMaintenancePrivilegeMissing (409)", err)
	}
	verdict, reason, detail := vpDecisionRow(t, ctx, admin, decisionID)
	if verdict != "blocked" || reason != reasonCapabilityMissing || detail == "" {
		t.Fatalf("decision = %s/%s/%q, want blocked capability_missing with a remedy",
			verdict, reason, detail)
	}
	var actions int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM sage.action_log
		WHERE sql_executed = $1`, sql).Scan(&actions); err != nil || actions != 0 {
		t.Fatalf("action_log rows for %q = %d (%v), want none", sql, actions, err)
	}
}

// Error propagation: a catalog read that fails withholds the action with
// that error; it never runs.
func TestApplyWithholdsOnPrecheckReadError(t *testing.T) {
	admin, ctx := requireDB(t)
	decisionID := vpDecision(t, ctx, admin, "a.b.c.d")
	e := New(admin, config.DefaultConfig(), time.Time{}, nopLog)
	executed, err := vpApply(ctx, e, "VACUUM a.b.c.d", decisionID)
	var withheld *WithheldError
	if executed || !errors.As(err, &withheld) ||
		withheld.Decision.BlockedReason != reasonCapabilityUnverified ||
		!strings.Contains(err.Error(), "too many dotted names") {
		t.Fatalf("executed=%v err=%v, want withheld with the catalog error", executed, err)
	}
	verdict, reason, detail := vpDecisionRow(t, ctx, admin, decisionID)
	if verdict != "blocked" || reason != reasonCapabilityUnverified ||
		!strings.Contains(detail, "too many dotted names") {
		t.Fatalf("decision = %s/%s/%q, want blocked capability_unverified", verdict, reason,
			detail)
	}
}

// vpVerifyAction records sql as executed with the baseline e measures now
// and returns the action id.
func vpVerifyAction(t *testing.T, ctx context.Context, e *Executor, admin *pgxpool.Pool,
	sql, table string) int64 {
	t.Helper()
	before := map[string]any{}
	e.predictAction(ctx, sql, nil, before)
	if _, ok := before[vacuumCountKey]; !ok {
		t.Fatalf("before_state = %v, want the %s baseline", before, vacuumCountKey)
	}
	raw, _ := json.Marshal(before)
	return insertVerifiedAction(t, admin, verifiedActionRow{sql: sql, before: string(raw),
		executedAt: time.Now().UTC(), decisionID: vpDecision(t, ctx, admin, table)})
}

// A VACUUM the server skipped (no privilege) is failed, not neutral.
func TestVerifyImmediateFailsSkippedVacuum(t *testing.T) {
	admin, ctx := requireDB(t)
	table := vpTable(t, ctx, admin, "vp_skip_t")
	deleteAndFlush(t, ctx, admin, "DELETE FROM "+table+" WHERE a > 500")
	waitForDeadTuples(t, ctx, admin, "vp_skip_t")
	pool, role := grantsRoleNamed(t, testdb.SkipUnlessLive(t), vpRoleName("skip"))
	vpGrantSage(t, ctx, admin, role)
	e := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	sql := "VACUUM " + table
	id := vpVerifyAction(t, ctx, e, admin, sql, table)
	vpVacuumSkipped(t, ctx, pool, table)

	e.verifyImmediate(ctx, id)

	var outcome, reason string
	if err := admin.QueryRow(ctx, `SELECT outcome, COALESCE(rollback_reason, '')
		FROM sage.action_log WHERE id=$1`, id).Scan(&outcome, &reason); err != nil {
		t.Fatalf("read action: %v", err)
	}
	if outcome != "failed" || !strings.Contains(reason, vacuumSkippedReason) {
		t.Fatalf("action = %s (%q), want failed: %s", outcome, reason, vacuumSkippedReason)
	}
	if got := verificationVerdict(t, admin, id); got != "failed" {
		t.Fatalf("verification verdict = %q, want failed", got)
	}
	got := storedOutcome(t, admin, id)
	if got.Verdict == verify.OutcomeNeutral || !strings.Contains(got.Reason, vacuumSkippedReason) {
		t.Fatalf("outcome = %+v, want not neutral, naming the skipped VACUUM", got)
	}
}

// A privileged VACUUM whose statistics report has not arrived (PostgreSQL
// 14's collector can drop it) is judged as before, never failed.
func TestVerifyImmediatePrivilegedUnreportedVacuumNotFailed(t *testing.T) {
	admin, ctx := requireDB(t)
	table := vpTable(t, ctx, admin, "vp_unreported_t")
	deleteAndFlush(t, ctx, admin, "DELETE FROM "+table+" WHERE a > 500")
	waitForDeadTuples(t, ctx, admin, "vp_unreported_t")
	e := New(admin, config.DefaultConfig(), time.Time{}, nopLog)
	id := vpVerifyAction(t, ctx, e, admin, "VACUUM "+table, table)

	e.verifyImmediate(ctx, id) // no VACUUM ran: vacuum_count cannot advance

	if outcome, _ := actionOutcomeFor(t, admin, id); outcome == "failed" {
		t.Fatalf("action outcome = failed, want the metric judgement for a privileged role")
	}
	if got := storedOutcome(t, admin, id); got.Verdict != verify.OutcomeNeutral {
		t.Fatalf("outcome = %+v, want neutral (dead tuples did not move)", got)
	}
}

// Review: a statement naming several tables is withheld even for a
// superuser (only one table can be checked), and a quoted name holding a
// space is checked whole, so a non-owner is refused rather than let through
// as "not found".
func TestApplyPrecheckReadsTheWholeTarget(t *testing.T) {
	admin, ctx := requireDB(t)
	first := vpTable(t, ctx, admin, "vp_multi_a")
	second := vpTable(t, ctx, admin, "vp_multi_b")
	e := New(admin, config.DefaultConfig(), time.Time{}, nopLog)
	// executor-08: a two-table VACUUM is no vacuum_table (no contract), so
	// the gate parks it before the maintenance precheck; here, past a
	// forced first decision, the precheck no longer applies and the
	// re-authorization (no standing gate installed) withholds it.
	multi := "VACUUM " + first + ", " + second
	if got := ActionTypeForSQL(multi); got != "" {
		t.Fatalf("two tables classify as %q, want no contract", got)
	}
	executed, err := vpApply(ctx, e, multi, vpDecision(t, ctx, admin, first))
	var withheld *WithheldError
	if executed || !errors.As(err, &withheld) || !withheld.Reauthorized ||
		withheld.Decision.BlockedReason != reasonNoStandingPolicy {
		t.Fatalf("two tables: executed=%v err=%v, want withheld at re-authorization "+
			"(%s), not by the maintenance precheck", executed, err, reasonNoStandingPolicy)
	}
	if _, err := admin.Exec(ctx, `DROP TABLE IF EXISTS public."vp spaced"; `+
		`CREATE TABLE public."vp spaced" (a int)`); err != nil {
		t.Fatalf("create spaced table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS public."vp spaced"`)
	})
	spaced := `"public"."vp spaced"`
	pool, role := grantsRoleNamed(t, testdb.SkipUnlessLive(t), vpRoleName("spaced"))
	vpGrantSage(t, ctx, admin, role)
	executed, err = vpApply(ctx, New(pool, config.DefaultConfig(), time.Time{}, nopLog),
		"VACUUM "+spaced, vpDecision(t, ctx, admin, "public.vp spaced"))
	if executed || !errors.As(err, &withheld) ||
		withheld.Decision.BlockedReason != reasonCapabilityMissing {
		t.Fatalf("spaced name: executed=%v err=%v, want withheld", executed, err)
	}
	ownerPassesPrecheck(t, ctx, e, spaced)
}

// ownerPassesPrecheck runs VACUUM table through Apply as e's role (the
// owner): with no standing gate the re-authorization refuses it, which
// proves the precheck before it admitted the statement.
func ownerPassesPrecheck(t *testing.T, ctx context.Context, e *Executor, table string) {
	t.Helper()
	_, err := vpApply(ctx, e, "VACUUM "+table, 0)
	var withheld *WithheldError
	if !errors.As(err, &withheld) || !withheld.Reauthorized ||
		withheld.Decision.BlockedReason == reasonCapabilityMissing {
		t.Fatalf("owner VACUUM %s = %v, want admitted by the precheck", table, err)
	}
}
