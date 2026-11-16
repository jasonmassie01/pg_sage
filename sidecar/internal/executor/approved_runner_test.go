package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// Approved queue items run through RunApprovedAction: an item a
// registered runner owns (a Sage SRE proposal) goes to that runner, with
// the approver; everything else keeps the manual path. A Sage SRE
// finding has no recommended SQL, so the manual path can never run it.

type fakeRunner struct {
	mu       sync.Mutex
	owns     bool
	calls    int
	approver int
	action   store.QueuedAction
	err      error
}

func (r *fakeRunner) Owns(a store.QueuedAction) bool { return r.owns }

func (r *fakeRunner) RunApproved(_ context.Context, a store.QueuedAction,
	approvedBy int) (ApprovedRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.approver, r.action = approvedBy, a
	if r.err != nil {
		return ApprovedRun{}, r.err
	}
	return ApprovedRun{ActionLogID: 99, VerificationStatus: "monitoring"}, nil
}

func queuedSRE() store.QueuedAction {
	return store.QueuedAction{ID: 5, FindingID: 8, ActionType: "cancel_backend",
		IdentityKey: "sre_proposal:x", ProposedSQL: "SELECT pg_cancel_backend(42)"}
}

func TestRunApprovedActionRoutesOwnedItemsToTheRunner(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	runner := &fakeRunner{owns: true}
	mustAddSRERunner(t, exec, runner)
	run, err := exec.RunApprovedAction(context.Background(), queuedSRE(), 12)
	if err != nil {
		t.Fatalf("RunApprovedAction: %v", err)
	}
	if run.ActionLogID != 99 || run.VerificationStatus != "monitoring" ||
		runner.calls != 1 || runner.approver != 12 || runner.action.ID != 5 {
		t.Fatalf("run = %+v, runner = %+v", run, runner)
	}
}

func TestRunApprovedActionPropagatesRunnerErrors(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	sentinel := errors.New("identity changed")
	mustAddSRERunner(t, exec, &fakeRunner{owns: true, err: sentinel})
	_, err := exec.RunApprovedAction(context.Background(), queuedSRE(), 12)
	if !errors.Is(err, sentinel) {
		t.Fatalf("runner error = %v, want the runner's error", err)
	}
}

func TestRunApprovedActionKeepsTheManualPathForOtherItems(t *testing.T) {
	for name, runner := range map[string]ApprovedActionRunner{
		"no runner": nil, "not owned": &fakeRunner{owns: false},
	} {
		exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
		if runner != nil {
			mustAddSRERunner(t, exec, runner)
		}
		action := store.QueuedAction{ID: 6, FindingID: 3, ProposedSQL: "DROP TABLE x"}
		_, err := exec.RunApprovedAction(context.Background(), action, 12)
		if err == nil || !strings.Contains(err.Error(), "SQL validation") {
			t.Fatalf("%s: RunApprovedAction = %v, want the manual path's validation",
				name, err)
		}
		if f, ok := runner.(*fakeRunner); ok && f.calls != 0 {
			t.Fatalf("%s: runner called %d times", name, f.calls)
		}
	}
}

func TestRunApprovedActionRejectsMissingApprover(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	runner := &fakeRunner{owns: true}
	mustAddSRERunner(t, exec, runner)
	if _, err := exec.RunApprovedAction(context.Background(), queuedSRE(), 0); !errors.Is(
		err, ErrBackendApprovalRequired) {
		t.Fatalf("approver 0 = %v, want ErrBackendApprovalRequired", err)
	}
	if runner.calls != 0 {
		t.Fatal("runner ran without an approver")
	}
}

// A runner can be registered while approvals run.
func TestAddApprovedActionRunnerIsRaceFree(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	runner := &fakeRunner{owns: true}
	var wg sync.WaitGroup
	wg.Add(9)
	go func() { defer wg.Done(); mustAddSRERunner(t, exec, runner) }()
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			_, _ = exec.RunApprovedAction(context.Background(), queuedSRE(), 1)
		}()
	}
	wg.Wait()
	if _, err := exec.RunApprovedAction(context.Background(), queuedSRE(), 1); err != nil {
		t.Fatalf("after concurrent registration: %v", err)
	}
}

// mustAddSRERunner registers runner for the Sage SRE proposal keys.
func mustAddSRERunner(t *testing.T, exec *Executor, runner ApprovedActionRunner) {
	t.Helper()
	if err := exec.AddApprovedActionRunner("sre", []string{"sre_proposal:"},
		runner); err != nil {
		t.Errorf("add the SRE runner: %v", err)
	}
}

func TestExecuteManualRefusesSageSREFindings(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := New(pool, advisoryConfig(), time.Time{}, nopLog)
	withTestStandingGate(exec)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	findingID := insertCancelFinding(t, pool)
	_, err := exec.ExecuteManual(ctx, findingID, "SELECT pg_cancel_backend(42)", "", nil)
	if !errors.Is(err, ErrFindingSQLMismatch) {
		t.Fatalf("manual run of a Sage SRE finding = %v, want ErrFindingSQLMismatch", err)
	}
}
