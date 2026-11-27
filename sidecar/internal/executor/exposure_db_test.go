package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

// fleetpol-02b on real PostgreSQL (PG14 and PG17): the typed revoke and
// RLS contracts, their exact rollback, the producer in shadow and enforce
// mode, and the reader verification that reverts a revoke a PUBLIC reader
// relied on.

type exposureFixture struct {
	pool  *pgxpool.Pool
	table string // bare name in public
	qual  string // public.<table>
}

func newExposureFixture(t *testing.T, grants ...string) exposureFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	name := fmt.Sprintf("exp_leads_%d", time.Now().UnixNano()%1e12)
	f := exposureFixture{pool: pool, table: name, qual: "public." + name}
	stmts := append([]string{"CREATE TABLE " + f.qual + " (id int, email text)",
		"INSERT INTO " + f.qual + " VALUES (1, 'a@example.test')"}, grants...)
	for _, sql := range stmts {
		if _, err := pool.Exec(ctx, strings.ReplaceAll(sql, "$T", f.qual)); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS "+f.qual)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.action_queue WHERE proposed_sql LIKE $1`,
			"%"+name+"%")
		_, _ = pool.Exec(ctx, `DELETE FROM sage.findings WHERE object_identifier = 'public'
			AND title = $1`, "exposure "+name)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.findings WHERE object_identifier = $1`, f.qual)
	})
	return f
}

func (f exposureFixture) acl(t *testing.T) string {
	t.Helper()
	var acl *string
	if err := f.pool.QueryRow(context.Background(), `SELECT relacl::text FROM pg_class
		WHERE oid = to_regclass($1)`, f.qual).Scan(&acl); err != nil {
		t.Fatalf("read relacl: %v", err)
	}
	if acl == nil {
		return "<null>"
	}
	return *acl
}

func (f exposureFixture) publicPrivileges(t *testing.T) string {
	t.Helper()
	var privs string
	if err := f.pool.QueryRow(context.Background(), `SELECT COALESCE(string_agg(
		privilege_type, ',' ORDER BY privilege_type), '') FROM pg_class c,
		aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
		WHERE c.oid = to_regclass($1) AND a.grantee = 0`, f.qual).Scan(&privs); err != nil {
		t.Fatalf("read PUBLIC privileges: %v", err)
	}
	return privs
}

// ap15Finding inserts an open AP-15 schema finding naming the table.
func (f exposureFixture) ap15Finding(t *testing.T) int64 {
	t.Helper()
	detail, _ := json.Marshal(map[string]any{"detector": "AP-15", "mode": "shadow",
		"evidence": []map[string]any{{"source": "pg_class.relacl", "ref": f.qual}}})
	var id int64
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, status)
		VALUES ('agent_posture:AP-15', 'warning', 'schema', 'public', $1, $2, 'open')
		RETURNING id`, "exposure "+f.table, detail).Scan(&id); err != nil {
		t.Fatalf("insert AP-15 finding: %v", err)
	}
	return id
}

func (f exposureFixture) ap03Finding(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, status)
		VALUES ('agent_posture:AP-03', 'critical', 'table', $1, 'exposed', '{}', 'open')
		RETURNING id`, f.qual).Scan(&id); err != nil {
		t.Fatalf("insert AP-03 finding: %v", err)
	}
	return id
}

func (f exposureFixture) analyzerFinding(detector string) analyzer.Finding {
	if detector == "AP-03" {
		return exposureFinding("AP-03", "table", f.qual)
	}
	finding := exposureFinding("AP-15", "schema", "public")
	finding.Detail["evidence"] = []any{map[string]any{"source": "pg_class.relacl",
		"ref": f.qual}}
	return finding
}

// exposureExecutor is an executor on pool whose standing policy has the
// exposure mode and whose trust runs moderate unattended when moderate
// (autonomous), else only with approval (advisory).
func exposureExecutor(pool *pgxpool.Pool, mode string, moderate bool) *Executor {
	cfg := windowedAutonomousConfig()
	if !moderate {
		cfg.Trust.Level = "advisory" // moderate runs only with approval
	}
	e := New(pool, cfg, time.Now().Add(-60*24*time.Hour), noopExecLog)
	e.WithEmergencyStopCheck(func(context.Context) bool { return false })
	e.SetExecutionMode("auto")
	e.WithActionStore(store.NewActionStore(pool), "")
	doc := unlimitedWindowPolicy()
	doc.ExposureMode = mode
	e.EnableStandingPolicyDocument(doc, nil)
	e.revokeWindow = 200 * time.Millisecond
	e.exposureHistory = func(context.Context, []int64, time.Time, time.Time) (
		map[int64]verify.Measurement, error) {
		return map[int64]verify.Measurement{}, nil // no history: nothing to judge
	}
	return e
}

func (f exposureFixture) actions(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT id, action_type, outcome,
		COALESCE(rollback_sql, ''), sql_executed, COALESCE(rollback_reason, '')
		FROM sage.action_log
		WHERE sql_executed LIKE $1 ORDER BY id`, "%"+f.table+"%")
	if err != nil {
		t.Fatalf("read actions: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id int64
		var kind, outcome, rollback, sql, reason string
		if err := rows.Scan(&id, &kind, &outcome, &rollback, &sql, &reason); err != nil {
			t.Fatal(err)
		}
		out = append(out, map[string]any{"id": id, "type": kind, "outcome": outcome,
			"rollback": rollback, "sql": sql, "reason": reason})
	}
	return out
}

func (f exposureFixture) queued(t *testing.T, status string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.action_queue
		WHERE proposed_sql LIKE $1 AND status = $2`, "%"+f.table+"%", status).Scan(&n); err != nil {
		t.Fatalf("count queue: %v", err)
	}
	return n
}

// Happy path: enforce mode plus a granted moderate tier revokes SELECT
// from PUBLIC, the post-check passes, the action row carries the exact
// re-GRANT and the remediation row, and the rollback restores relacl.
func TestRevokePublic_EnforceRevokesAndRollbackRestoresExactACL(t *testing.T) {
	f := newExposureFixture(t, "GRANT SELECT ON $T TO PUBLIC")
	before := f.acl(t)
	findingID := f.ap15Finding(t)
	e := exposureExecutor(f.pool, policy.ExposureModeEnforce, true)
	ctx := context.Background()
	e.runExposureRemediations(ctx, false)
	if got := f.publicPrivileges(t); got != "" {
		t.Fatalf("PUBLIC still holds %q after enforce", got)
	}
	acts := f.actions(t)
	if len(acts) != 1 || acts[0]["type"] != ActionTypeRevokePublicGrant {
		t.Fatalf("actions = %+v, want one revoke_public_grant", acts)
	}
	wantRollback := "GRANT SELECT ON TABLE " + f.qual + " TO PUBLIC"
	if acts[0]["rollback"] != wantRollback {
		t.Fatalf("rollback_sql = %q, want %q", acts[0]["rollback"], wantRollback)
	}
	var object string
	var revoked []string
	if err := f.pool.QueryRow(ctx, `SELECT object, ARRAY(SELECT jsonb_array_elements_text(
		revoked)) FROM sage.exposure_remediations WHERE action_log_id = $1`,
		acts[0]["id"]).Scan(&object, &revoked); err != nil {
		t.Fatalf("read exposure_remediations: %v", err)
	}
	if object != f.qual || strings.Join(revoked, ",") != "SELECT" {
		t.Fatalf("remediation = %s %v", object, revoked)
	}
	var fid int64
	_ = f.pool.QueryRow(ctx, `SELECT finding_id FROM sage.action_log WHERE id=$1`,
		acts[0]["id"]).Scan(&fid)
	if fid != findingID {
		t.Fatalf("action finding_id = %d, want the AP-15 finding %d", fid, findingID)
	}
	if err := ExecInTransaction(ctx, f.pool, wantRollback, 5*time.Second); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if after := f.acl(t); after != before {
		t.Fatalf("relacl after rollback = %s, want the exact prior %s", after, before)
	}
}

// Nil/empty: a NULL ACL (default privileges) is read with acldefault and
// leaves nothing to revoke: no_change, no action row, no proposal.
func TestRevokePublic_NullACLIsNoChange(t *testing.T) {
	f := newExposureFixture(t)
	if f.acl(t) != "<null>" {
		t.Fatalf("fixture ACL = %s, want NULL", f.acl(t))
	}
	f.ap15Finding(t)
	e := exposureExecutor(f.pool, policy.ExposureModeEnforce, true)
	e.runExposureRemediations(context.Background(), false)
	if acts := f.actions(t); len(acts) != 0 {
		t.Fatalf("actions = %+v, want none for nothing to revoke", acts)
	}
	if n := f.queued(t, "pending"); n != 0 {
		t.Fatalf("%d proposals, want none", n)
	}
	stmt, _ := parseExposureSQL("REVOKE ALL ON TABLE " + f.qual + " FROM PUBLIC")
	finding := f.analyzerFinding("AP-15")
	finding.RecommendedSQL = "REVOKE ALL ON TABLE " + f.qual + " FROM PUBLIC"
	err := e.execExposure(context.Background(), &finding, stmt, map[string]any{}, 2000)
	if !errors.Is(err, ErrExposureNoChange) {
		t.Fatalf("exec on a NULL ACL = %v, want ErrExposureNoChange", err)
	}
}

// Boundary and state transitions: shadow never proposes; enforce with
// moderate not granted proposes for approval; back to shadow withdraws the
// pending proposal; an executed revoke stays with its rollback.
func TestRevokePublic_ShadowEnforceShadow(t *testing.T) {
	f := newExposureFixture(t, "GRANT SELECT, UPDATE ON $T TO PUBLIC")
	f.ap15Finding(t)
	ctx := context.Background()
	shadow := exposureExecutor(f.pool, "", false)
	shadow.runExposureRemediations(ctx, false)
	if n := f.queued(t, "pending"); n != 0 || len(f.actions(t)) != 0 {
		t.Fatalf("shadow proposed %d / acted %d, want nothing", n, len(f.actions(t)))
	}
	var decisions int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM sage.decision
		WHERE target_objects::text LIKE $1`, "%"+f.table+"%").Scan(&decisions); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if decisions != 0 {
		t.Fatalf("shadow recorded %d decisions, want none", decisions)
	}
	enforce := exposureExecutor(f.pool, policy.ExposureModeEnforce, false)
	enforce.runExposureRemediations(ctx, false)
	if n := f.queued(t, "pending"); n != 1 || len(f.actions(t)) != 0 {
		t.Fatalf("enforce below moderate: %d pending, %d actions; want 1 and 0",
			n, len(f.actions(t)))
	}
	if f.publicPrivileges(t) != "SELECT,UPDATE" {
		t.Fatal("a queued proposal must not change the ACL")
	}
	shadow.runExposureRemediations(ctx, false)
	if f.queued(t, "pending") != 0 || f.queued(t, "superseded") != 1 {
		t.Fatalf("back to shadow: pending %d superseded %d, want 0 and 1",
			f.queued(t, "pending"), f.queued(t, "superseded"))
	}
	granted := exposureExecutor(f.pool, policy.ExposureModeEnforce, true)
	granted.runExposureRemediations(ctx, false)
	acts := f.actions(t)
	if len(acts) != 1 {
		t.Fatalf("granted enforce actions = %+v", acts)
	}
	shadow.runExposureRemediations(ctx, false)
	if again := f.actions(t); len(again) != 1 || again[0]["rollback"] !=
		"GRANT SELECT, UPDATE ON TABLE "+f.qual+" TO PUBLIC" {
		t.Fatalf("after shadow the executed revoke = %+v, want it kept with its rollback",
			again)
	}
}

// Concurrent access: two cycles proposing the same revoke produce one
// action (the apply lease and the open-finding claim).
func TestRevokePublic_TwoCyclesOneAction(t *testing.T) {
	f := newExposureFixture(t, "GRANT SELECT ON $T TO PUBLIC")
	f.ap15Finding(t)
	a := exposureExecutor(f.pool, policy.ExposureModeEnforce, true)
	b := exposureExecutor(f.pool, policy.ExposureModeEnforce, true)
	var wg sync.WaitGroup
	for _, e := range []*Executor{a, b} {
		wg.Add(1)
		go func(e *Executor) {
			defer wg.Done()
			e.runExposureRemediations(context.Background(), false)
		}(e)
	}
	wg.Wait()
	var ok int
	for _, act := range f.actions(t) {
		if act["outcome"] != "failed" {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("actions = %+v, want exactly one executed revoke", f.actions(t))
	}
	if f.publicPrivileges(t) != "" {
		t.Fatal("PUBLIC still holds SELECT")
	}
}

// Error propagation: a held lock ends in a retryable error naming the
// lock; a role that is not the owner ends the proposal with the ownership
// message and is never retried.
func TestRevokePublic_LockTimeoutAndNotOwner(t *testing.T) {
	f := newExposureFixture(t, "GRANT SELECT ON $T TO PUBLIC")
	ctx := context.Background()
	e := exposureExecutor(f.pool, policy.ExposureModeEnforce, true)
	stmt, _ := parseExposureSQL("REVOKE ALL ON TABLE " + f.qual + " FROM PUBLIC")
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, "LOCK TABLE "+f.qual+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	finding := f.analyzerFinding("AP-15")
	err = e.execExposure(ctx, &finding, stmt, map[string]any{}, 300)
	_ = holder.Rollback(ctx)
	if !errors.Is(err, ErrLockNotAvailable) || !strings.Contains(err.Error(), f.table) {
		t.Fatalf("held lock = %v, want ErrLockNotAvailable naming %s", err, f.table)
	}
	if errors.Is(err, ErrExposureNotOwner) {
		t.Fatal("a lock timeout is retryable, not an ownership refusal")
	}
	actor := exposureActor(t, f)
	e.WithActionPool(actor)
	err = e.execExposure(ctx, &finding, stmt, map[string]any{}, 2000)
	if !errors.Is(err, ErrExposureNotOwner) ||
		!strings.Contains(err.Error(), "grant ownership or run as the owner") {
		t.Fatalf("non-owner = %v, want the ownership refusal", err)
	}
	f.ap15Finding(t)
	for range 2 {
		e.runExposureRemediations(ctx, false)
	}
	acts := f.actions(t)
	if len(acts) != 1 || acts[0]["outcome"] != "failed" {
		t.Fatalf("non-owner actions = %+v, want one failed refusal and no retry", acts)
	}
	if f.publicPrivileges(t) != "SELECT" {
		t.Fatal("a refused revoke changed the ACL")
	}
}

// exposureActor is a pool logged in as a role that does not own the table.
func exposureActor(t *testing.T, f exposureFixture) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	role := "exp_actor_" + f.table[len(f.table)-6:]
	for _, sql := range []string{"DROP ROLE IF EXISTS " + role,
		"CREATE ROLE " + role + " LOGIN PASSWORD 'exp-actor'",
		"GRANT USAGE ON SCHEMA public TO " + role} {
		if _, err := f.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, "exp-actor"
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Close()
		_, _ = f.pool.Exec(context.Background(), "DROP OWNED BY "+role)
		_, _ = f.pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	return p
}

// enable_row_level_security: in enforce mode at the highest trust level it
// is proposed, never executed; the card lists the roles losing access; the
// approved run enables RLS and its rollback disables it again.
func TestEnableRLS_ProposedWithCardThenApprovedAndRolledBack(t *testing.T) {
	f := newExposureFixture(t, "GRANT SELECT ON $T TO PUBLIC")
	findingID := f.ap03Finding(t)
	ctx := context.Background()
	e := exposureExecutor(f.pool, policy.ExposureModeEnforce, true)
	e.runExposureRemediations(ctx, false)
	if len(f.actions(t)) != 0 || f.queued(t, "pending") != 1 {
		t.Fatalf("RLS at full trust: actions %d pending %d, want a proposal only",
			len(f.actions(t)), f.queued(t, "pending"))
	}
	var rollback, evidence string
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(q.rollback_sql, ''),
		COALESCE((SELECT d.evidence::text FROM sage.decision d
		  WHERE d.queue_id = q.id ORDER BY d.id DESC LIMIT 1), '')
		FROM sage.action_queue q
		WHERE q.proposed_sql LIKE $1 AND q.status = 'pending'`, "%"+f.table+"%").
		Scan(&rollback, &evidence); err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	if rollback != "ALTER TABLE "+f.qual+" DISABLE ROW LEVEL SECURITY" {
		t.Fatalf("proposal rollback = %q", rollback)
	}
	if !strings.Contains(evidence, "roles_losing_access") ||
		!strings.Contains(evidence, "PUBLIC") || !strings.Contains(evidence, "CREATE POLICY") {
		t.Fatalf("the queuing decision lacks the approval card: %s", evidence)
	}
	sql := "ALTER TABLE " + f.qual + " ENABLE ROW LEVEL SECURITY"
	approver := 1
	if _, err := e.ExecuteManual(ctx, int(findingID), sql, rollback, &approver); err != nil {
		t.Fatalf("approved RLS enable: %v", err)
	}
	var on bool
	_ = f.pool.QueryRow(ctx, `SELECT relrowsecurity FROM pg_class WHERE oid=to_regclass($1)`,
		f.qual).Scan(&on)
	if !on {
		t.Fatal("RLS is not enabled after the approved run")
	}
	if err := ExecInTransaction(ctx, f.pool, rollback, 5*time.Second); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	_ = f.pool.QueryRow(ctx, `SELECT relrowsecurity FROM pg_class WHERE oid=to_regclass($1)`,
		f.qual).Scan(&on)
	if on {
		t.Fatal("RLS still enabled after its rollback")
	}
}

// Verification: a reader that relied on PUBLIC stops succeeding after the
// revoke; its statement ran in the hour before and not in the window
// after, so the verifier reverts and the ACL is back.
func TestRevokePublic_VerifierRevertsWhenAReaderStops(t *testing.T) {
	f := newExposureFixture(t, "GRANT SELECT ON $T TO PUBLIC")
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("pg_stat_statements is required on the test server: %v", err)
	}
	before := f.acl(t)
	reader := exposureActor(t, f) // holds no grant: reads through PUBLIC only
	readSQL := "SELECT count(*) FROM " + f.qual
	var n int
	for range 3 {
		if err := reader.QueryRow(ctx, readSQL).Scan(&n); err != nil {
			t.Fatalf("reader before the revoke: %v", err)
		}
	}
	f.ap15Finding(t)
	e := exposureExecutor(f.pool, policy.ExposureModeEnforce, true)
	e.exposureHistory = func(_ context.Context, ids []int64, _, _ time.Time) (
		map[int64]verify.Measurement, error) {
		out := map[int64]verify.Measurement{}
		for _, id := range ids {
			out[id] = verify.Measurement{Samples: 3} // ran in the hour before
		}
		return out, nil
	}
	e.runExposureRemediations(ctx, false)
	if err := reader.QueryRow(ctx, readSQL).Scan(&n); err == nil {
		t.Fatal("the PUBLIC reader still succeeds after the revoke")
	}
	// The revert's GRANT commits before its outcome is recorded: wait for
	// both, or the outcome can still read rolling_back.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && (f.acl(t) != before || !rolledBack(f.actions(t))) {
		time.Sleep(100 * time.Millisecond)
	}
	if after := f.acl(t); after != before {
		t.Fatalf("ACL after verification = %s, want reverted to %s", after, before)
	}
	acts := f.actions(t)
	if len(acts) != 1 || acts[0]["outcome"] != "rolled_back" {
		t.Fatalf("actions = %+v, want the revoke rolled back", acts)
	}
	if err := reader.QueryRow(ctx, readSQL).Scan(&n); err != nil {
		t.Fatalf("reader after the revert: %v", err)
	}
}

// rolledBack reports whether the only action recorded is rolled back.
func rolledBack(acts []map[string]any) bool {
	return len(acts) == 1 && acts[0]["outcome"] == "rolled_back"
}
