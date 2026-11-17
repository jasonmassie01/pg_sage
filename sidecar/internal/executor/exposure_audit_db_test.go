package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// fleetpol-02b post-test audit: the operator-approved revoke records the
// exact re-GRANT and its remediation row, and a revoke without statement
// history is kept but never credited as verified.

func TestRevokePublic_ApprovedProposalRunsWithExactRollback(t *testing.T) {
	f := newExposureFixture(t, "GRANT SELECT, UPDATE ON $T TO PUBLIC")
	findingID := f.ap15Finding(t)
	ctx := context.Background()
	e := exposureExecutor(f.pool, policy.ExposureModeEnforce, false) // advisory
	e.runExposureRemediations(ctx, false)
	var proposed string
	if err := f.pool.QueryRow(ctx, `SELECT proposed_sql FROM sage.action_queue
		WHERE proposed_sql LIKE $1 AND status = 'pending'`, "%"+f.table+"%").
		Scan(&proposed); err != nil {
		t.Fatalf("read the proposal: %v", err)
	}
	approver := 1
	actionID, err := e.ExecuteManual(ctx, int(findingID), proposed, "", &approver)
	if err != nil {
		t.Fatalf("approved revoke: %v", err)
	}
	if got := f.publicPrivileges(t); got != "" {
		t.Fatalf("PUBLIC still holds %q after the approved revoke", got)
	}
	var rollback string
	var revoked int
	if err := f.pool.QueryRow(ctx, `SELECT COALESCE(a.rollback_sql, ''),
		(SELECT jsonb_array_length(r.revoked) FROM sage.exposure_remediations r
		  WHERE r.action_log_id = a.id)
		FROM sage.action_log a WHERE a.id = $1`, actionID).Scan(&rollback, &revoked); err != nil {
		t.Fatalf("read the action: %v", err)
	}
	if want := "GRANT SELECT, UPDATE ON TABLE " + f.qual + " TO PUBLIC"; rollback != want ||
		revoked != 2 {
		t.Fatalf("rollback = %q revoked = %d, want %q and 2", rollback, revoked, want)
	}
	// Invalid input: an operator cannot run the re-GRANT forward.
	if _, err := e.ExecuteManual(ctx, int(findingID), rollback, "", &approver); err == nil {
		t.Fatal("a forward re-GRANT through ExecuteManual was accepted")
	}
	if f.publicPrivileges(t) != "" {
		t.Fatal("a refused re-GRANT changed the ACL")
	}
}

func TestRevokePublic_NoHistoryIsKeptUnverifiable(t *testing.T) {
	f := newExposureFixture(t, "GRANT SELECT ON $T TO PUBLIC")
	f.ap15Finding(t)
	ctx := context.Background()
	e := exposureExecutor(f.pool, policy.ExposureModeEnforce, true)
	e.exposureHistory = func(context.Context, []int64, time.Time, time.Time) (
		map[int64]verify.Measurement, error) {
		return nil, errors.New("history store unavailable")
	}
	e.runExposureRemediations(ctx, false)
	var outcome string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		acts := f.actions(t)
		if len(acts) == 1 && acts[0]["outcome"] != "monitoring" {
			outcome = acts[0]["outcome"].(string)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if outcome != "unverifiable" {
		t.Fatalf("outcome = %q (%+v), want unverifiable", outcome, f.actions(t))
	}
	if f.publicPrivileges(t) != "" {
		t.Fatal("an unverifiable revoke must be kept, not reverted")
	}
}

// Without pg_stat_statements there is no reader evidence: the revoke is
// kept but settles unverifiable, never success (no trust credit without
// evidence; fleetpol-02b review).
func TestRevokePublic_NoStatementsViewIsKeptUnverifiable(t *testing.T) {
	f := newExposureFixture(t, "GRANT SELECT ON $T TO PUBLIC")
	ctx := context.Background()
	var installed bool
	if err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension
		WHERE extname = 'pg_stat_statements')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, "DROP EXTENSION IF EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("drop pg_stat_statements: %v", err)
	}
	if installed {
		t.Cleanup(func() {
			_, _ = f.pool.Exec(context.Background(),
				"CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
		})
	}
	f.ap15Finding(t)
	e := exposureExecutor(f.pool, policy.ExposureModeEnforce, true)
	e.runExposureRemediations(ctx, false)
	var outcome string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		acts := f.actions(t)
		if len(acts) == 1 && acts[0]["outcome"] != "monitoring" {
			outcome = acts[0]["outcome"].(string)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if outcome != "unverifiable" {
		t.Fatalf("outcome = %q (%+v), want unverifiable", outcome, f.actions(t))
	}
	if f.publicPrivileges(t) != "" {
		t.Fatal("an unverifiable revoke must be kept, not reverted")
	}
}
