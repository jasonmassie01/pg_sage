package cases

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// migration-05: a migration finding escalated by the lock queue behind
// its DDL carries an approval-only cancel of the DDL backend: a guarded
// cancel_backend script at HIGH risk, never queued for execution.

func lockQueueFinding(t *testing.T, detail map[string]any) SourceFinding {
	t.Helper()
	// Round-trip through JSON as sage.findings.detail does.
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	return SourceFinding{ID: "f1", DatabaseName: "db1", Category: "migration_safety",
		Severity: SeverityCritical, ObjectIdentifier: "public.orders:ddl_lock_queue:x",
		RuleID: "ddl_lock_queue", Detail: back, ObservedAt: time.Now()}
}

func escalatedDetail() map[string]any {
	start := time.Date(2026, 11, 8, 9, 0, 0, 123000, time.UTC)
	return map[string]any{
		"pid": 200, "backend_start": start, "query_start": start.Add(time.Minute),
		"query_sha": "0123456789abcdef", "state": "active",
		"lock_queue_escalated": true, "lock_queue_waiters": 3, "risk_score": 0.2,
	}
}

func cancelCandidate(cands []ActionCandidate) *ActionCandidate {
	for i := range cands {
		if cands[i].ActionType == "cancel_backend" {
			return &cands[i]
		}
	}
	return nil
}

func TestMigrationLockQueue_CancelCandidateIsHighScriptOnly(t *testing.T) {
	cands := actionCandidatesForFinding(lockQueueFinding(t, escalatedDetail()), testClassify)
	c := cancelCandidate(cands)
	if c == nil {
		t.Fatalf("no cancel_backend candidate in %+v", cands)
	}
	if c.RiskTier != "high" || c.BlockedReason != "" {
		t.Fatalf("risk %q blocked %q, want high and unblocked", c.RiskTier, c.BlockedReason)
	}
	if len(c.OutputModes) != 1 || c.OutputModes[0] != "script" {
		t.Fatalf("output modes %v, want script only (never queued)", c.OutputModes)
	}
	if c.BackendIdentity == nil || c.BackendIdentity.PID != 200 ||
		c.BackendIdentity.State != "active" {
		t.Fatalf("backend identity = %+v", c.BackendIdentity)
	}
	for _, s := range []string{"pg_cancel_backend(a.pid)", "WHERE a.pid = 200",
		"a.backend_start = '2026-11-08T09:00:00.000123Z'", "0123456789abcdef"} {
		if !strings.Contains(c.ProposedSQL, s) {
			t.Fatalf("guarded script lacks %q:\n%s", s, c.ProposedSQL)
		}
	}
	if strings.Contains(c.ProposedSQL, "pg_terminate_backend") {
		t.Fatalf("lock-queue guidance must cancel, not terminate:\n%s", c.ProposedSQL)
	}
}

// Nil/empty and invalid input: no escalation, or an incomplete identity,
// gives no runnable cancel.
func TestMigrationLockQueue_NoCancelWithoutCompleteIdentity(t *testing.T) {
	plain := escalatedDetail()
	delete(plain, "lock_queue_escalated")
	if c := cancelCandidate(actionCandidatesForFinding(lockQueueFinding(t, plain),
		testClassify)); c != nil {
		t.Fatalf("unescalated finding got a cancel: %+v", c)
	}
	for _, key := range []string{"backend_start", "query_start", "query_sha", "pid"} {
		d := escalatedDetail()
		delete(d, key)
		c := cancelCandidate(actionCandidatesForFinding(lockQueueFinding(t, d), testClassify))
		if c == nil || c.ProposedSQL != "" || c.BlockedReason == "" {
			t.Fatalf("missing %s: candidate %+v, want a blocked cancel with no SQL", key, c)
		}
	}
	bad := escalatedDetail()
	bad["query_sha"] = "not-a-sha"
	c := cancelCandidate(actionCandidatesForFinding(lockQueueFinding(t, bad), testClassify))
	if c == nil || c.ProposedSQL != "" {
		t.Fatalf("malformed query_sha produced SQL: %+v", c)
	}
}
