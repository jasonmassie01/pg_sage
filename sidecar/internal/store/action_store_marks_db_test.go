//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/recommendation"
)

// config-32: the queue state machine reads the same after the approval,
// marks and reads move into their own files.

func proposeForTest(
	t *testing.T, pool *pgxpool.Pool, s *ActionStore, findingID int, sql string,
) int {
	t.Helper()
	id, err := s.Propose(context.Background(), nil, findingID, sql, "", "safe")
	if err != nil {
		t.Fatalf("Propose %q: %v", sql, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.action_queue WHERE id = $1", id)
	})
	return id
}

func insertActionLogForTest(
	t *testing.T, pool *pgxpool.Pool, findingID int, sql string,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		    (action_type, finding_id, sql_executed, outcome)
		 VALUES ('analyze', $1, $2, 'success') RETURNING id`,
		findingID, sql).Scan(&id)
	if err != nil {
		t.Fatalf("insert action log: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `UPDATE sage.action_queue SET action_log_id = NULL
			WHERE action_log_id = $1`, id)
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_log WHERE id = $1", id)
	})
	return id
}

func TestActionQueue_ApproveExecuteThenReadinessRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)
	findingID := insertActionFinding(t, pool, ctx, "split_execute")
	id := proposeForTest(t, pool, s, findingID, "ANALYZE public.split_execute")

	approved, err := s.Approve(ctx, id, 1)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != "approved" || approved.DecidedBy == nil ||
		*approved.DecidedBy != 1 || approved.DecidedAt == nil {
		t.Fatalf("approved row = %+v", approved)
	}
	logID := insertActionLogForTest(t, pool, findingID, "ANALYZE public.split_execute")
	if err := s.MarkExecuted(ctx, id, logID, ""); err != nil {
		t.Fatalf("MarkExecuted: %v", err)
	}
	got, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != "executed" || got.ActionLogID == nil || *got.ActionLogID != logID ||
		got.VerificationStatus != "verified" || got.CooldownUntil != nil ||
		got.LastAttemptAt == nil {
		t.Fatalf("executed row = %+v", got)
	}
	err = s.MarkReadinessOutcome(ctx, id, "expired", "too late")
	want := fmt.Sprintf("action %d not found or already executed", id)
	if err == nil || err.Error() != want {
		t.Fatalf("readiness on executed = %v, want %q", err, want)
	}
	if after, _ := s.GetByID(ctx, id); after.Status != "executed" {
		t.Fatalf("status after refused readiness = %q, want executed", after.Status)
	}
	if err := s.MarkExecuted(ctx, -1, 1, "verified"); err == nil ||
		err.Error() != "action -1 not found" {
		t.Fatalf("MarkExecuted on a missing row = %v", err)
	}
}

func TestActionQueue_ReadinessOutcomeOnPending(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)
	findingID := insertActionFinding(t, pool, ctx, "split_readiness")
	id := proposeForTest(t, pool, s, findingID, "ANALYZE public.split_readiness")

	if err := s.MarkReadinessOutcome(ctx, id, "blocked", "gate closed"); err != nil {
		t.Fatalf("MarkReadinessOutcome: %v", err)
	}
	got, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != "blocked" || got.Reason != "gate closed" || got.DecidedBy != nil {
		t.Fatalf("readiness row = status %q reason %q decided_by %v",
			got.Status, got.Reason, got.DecidedBy)
	}
}

func TestActionQueue_RejectThenRefusedApproval(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)
	findingID := insertActionFinding(t, pool, ctx, "split_reject")
	id := proposeForTest(t, pool, s, findingID, "ANALYZE public.split_reject")

	if err := s.Reject(ctx, id, 1, "not now"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	got, _ := s.GetByID(ctx, id)
	if got.Status != "rejected" || got.Reason != "not now" || got.DecidedAt == nil {
		t.Fatalf("rejected row = %+v", got)
	}
	rejected, err := s.HasRecentlyRejectedForFinding(ctx, findingID, time.Hour)
	if err != nil || !rejected {
		t.Fatalf("HasRecentlyRejectedForFinding = (%v, %v), want true", rejected, err)
	}
	if other, err := s.HasRecentlyRejectedForFinding(ctx, -5, time.Hour); err != nil || other {
		t.Fatalf("HasRecentlyRejectedForFinding(other) = (%v, %v), want false", other, err)
	}
	_, err = s.Approve(ctx, id, 2)
	if !errors.Is(err, pgx.ErrNoRows) || errors.Is(err, recommendation.ErrRevised) {
		t.Fatalf("approving a rejected row = %v, want ErrNoRows and not ErrRevised", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET status = 'superseded'
		WHERE id = $1`, id); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	_, err = s.Approve(ctx, id, 2)
	if !errors.Is(err, recommendation.ErrRevised) ||
		!strings.Contains(err.Error(), fmt.Sprintf("queued proposal %d was superseded", id)) {
		t.Fatalf("approving a superseded row = %v, want ErrRevised", err)
	}
	if after, _ := s.GetByID(ctx, id); after.Status != "superseded" || *after.DecidedBy != 1 {
		t.Fatalf("refused approval changed the row: %+v", after)
	}
}

func TestListLedgerByFindingIDs_GroupsNewestTwentyPerFinding(t *testing.T) {
	pool, ctx := requireDB(t)
	s := NewActionStore(pool)
	busy := insertActionFinding(t, pool, ctx, "split_ledger_busy")
	quiet := insertActionFinding(t, pool, ctx, "split_ledger_quiet")
	var busyIDs []int
	for i := 0; i < 21; i++ {
		busyIDs = append(busyIDs, proposeForTest(t, pool, s, busy,
			fmt.Sprintf("ANALYZE public.split_ledger_%d", i)))
	}
	quietID := proposeForTest(t, pool, s, quiet, "ANALYZE public.split_ledger_quiet")
	executed := proposeForTest(t, pool, s, quiet, "ANALYZE public.split_ledger_done")
	logID := insertActionLogForTest(t, pool, quiet, "ANALYZE public.split_ledger_done")
	if err := s.MarkExecuted(ctx, executed, logID, "verified"); err != nil {
		t.Fatalf("MarkExecuted: %v", err)
	}

	got, err := s.ListLedgerByFindingIDs(ctx, []int{busy, quiet, -9})
	if err != nil {
		t.Fatalf("ListLedgerByFindingIDs: %v", err)
	}
	if len(got) != 2 || len(got[busy]) != 20 || len(got[quiet]) != 1 {
		t.Fatalf("ledger sizes = busy %d quiet %d (%d findings), want 20, 1, 2",
			len(got[busy]), len(got[quiet]), len(got))
	}
	if got[busy][0].ID != busyIDs[20] || got[busy][19].ID != busyIDs[1] {
		t.Fatalf("busy ledger order = first %d last %d, want %d then %d",
			got[busy][0].ID, got[busy][19].ID, busyIDs[20], busyIDs[1])
	}
	q := got[quiet][0]
	if q.ID != quietID || q.Status != "pending" || q.ProposedSQL !=
		"ANALYZE public.split_ledger_quiet" || q.VerificationStatus != "not_started" {
		t.Fatalf("quiet ledger row = %+v", q)
	}
}
