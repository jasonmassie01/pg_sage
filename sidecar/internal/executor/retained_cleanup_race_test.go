package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

type cleanupRaceGate struct {
	replace func()
}

func (g cleanupRaceGate) Authorize(context.Context, policy.ActionRequest) policy.Decision {
	g.replace()
	return policy.Decision{Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe}
}

// Policy evaluation deterministically schedules external DDL after identity validation.
func TestRetainedCleanupPreservesIndexReplacedAfterValidation(t *testing.T) {
	e, id, _ := retainedFixture(t, true, "")
	var replacementOID int64
	e.WithPolicyGate(cleanupRaceGate{replace: func() {
		_, err := e.pool.Exec(t.Context(), `DROP INDEX public.cleanup_old;
			CREATE INDEX cleanup_old ON public.cleanup_orders(value)`)
		if err != nil {
			t.Fatal(err)
		}
		err = e.pool.QueryRow(t.Context(), `SELECT 'public.cleanup_old'::regclass::bigint`).
			Scan(&replacementOID)
		if err != nil {
			t.Fatal(err)
		}
	}})
	err := (&executorIndexActions{exec: e}).Retain(t.Context(), id, verify.Verdict{Retain: true})
	var currentOID int64
	lookupErr := e.pool.QueryRow(t.Context(),
		`SELECT COALESCE(to_regclass('public.cleanup_old')::bigint,0)`).Scan(&currentOID)
	if lookupErr != nil || err == nil || currentOID == 0 || currentOID != replacementOID {
		t.Fatalf("replacement lost: oid=%d want=%d retain=%v lookup=%v",
			currentOID, replacementOID, err, lookupErr)
	}
	assertCleanupReviewReason(t, e, id)
}

func TestRetainedCleanupReviewRequiresDurableVerification(t *testing.T) {
	e, id, _ := retainedFixture(t, true, "")
	if _, err := e.pool.Exec(t.Context(),
		"DELETE FROM sage.verification WHERE action_log_id=$1", id); err != nil {
		t.Fatal(err)
	}
	err := e.cleanupRetainedIndex(t.Context(), id)
	if err == nil || !strings.Contains(err.Error(), "verification missing") {
		t.Fatalf("missing review persistence was not actionable: %v", err)
	}
	assertCleanupIndexes(t, e, false)
}

func TestRetainedCleanupReviewWritePreservesCancellation(t *testing.T) {
	e, id, _ := retainedFixture(t, true, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.WithPolicyGate(cleanupRaceGate{replace: cancel})
	err := e.cleanupRetainedIndex(ctx, id)
	if !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "record superseded cleanup review") {
		t.Fatalf("lost review persistence failure: %v", err)
	}
	assertCleanupIndexes(t, e, false)
}

func TestRetainedCleanupReviewDoesNotRevertOrRescheduleSuccessfulWatch(t *testing.T) {
	e, id, _ := retainedFixture(t, true, "")
	verifier := &fakeIndexVerifier{watchVerdict: verify.Verdict{Retain: true, Status: "success"}}
	lifecycle := newVerifiedIndexLifecycle(verifier, &executorIndexActions{exec: e})
	err := lifecycle.WatchApplied(t.Context(), verifiedIndexAction{
		RollbackSQL: "DROP INDEX CONCURRENTLY public.cleanup_new", WatchID: "reviewed-cleanup",
	}, id)
	if !errors.Is(err, ErrReviewedCleanupRequired) {
		t.Fatalf("lifecycle lost reviewed-cleanup distinction: %v", err)
	}
	assertCleanupIndexes(t, e, false)
	assertCleanupReviewReason(t, e, id)
	due, err := verify.NewPostgresStateStore(e.pool, 1).ListDue(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, watch := range due {
		if watch.ActionID == id {
			t.Fatal("successful retained watch rescheduled after cleanup review was withheld")
		}
	}
}
