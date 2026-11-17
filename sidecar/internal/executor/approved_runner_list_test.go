package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// plan-02a: the approved-action runner slot is an ordered list. Each
// runner names the identity-key prefixes it owns; two runners whose
// prefixes overlap are a startup error naming both, so no item is ever
// owned twice. A plan_step: item runs only through the plan runner: with
// none registered it is refused, never sent down the manual path.

func queuedPlanStep() store.QueuedAction {
	return store.QueuedAction{ID: 7, FindingID: 3, IdentityKey: "plan_step:4:9",
		ProposedBy: "plan:4", ProposedSQL: "CREATE INDEX CONCURRENTLY i ON t (a)"}
}

func queuedMaintenance() store.QueuedAction {
	return store.QueuedAction{ID: 8, FindingID: 3, IdentityKey: "sre_maintenance:x",
		ProposedSQL: "VACUUM t"}
}

func TestAddApprovedActionRunner_DisjointRunnersCoexist(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	plans, sre := &fakeRunner{owns: true}, &fakeRunner{owns: true}
	if err := exec.AddApprovedActionRunner("plans", []string{PlanStepIdentityPrefix},
		plans); err != nil {
		t.Fatalf("add plans: %v", err)
	}
	if err := exec.AddApprovedActionRunner("sre", []string{"sre_maintenance:",
		"sre_proposal:"}, sre); err != nil {
		t.Fatalf("add sre: %v", err)
	}
	ctx := context.Background()
	if _, err := exec.RunApprovedAction(ctx, queuedPlanStep(), 3); err != nil {
		t.Fatalf("plan step: %v", err)
	}
	if _, err := exec.RunApprovedAction(ctx, queuedMaintenance(), 4); err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	if plans.calls != 1 || plans.action.ID != 7 || plans.approver != 3 {
		t.Fatalf("plan runner %+v, want item 7 for approver 3 once", plans)
	}
	if sre.calls != 1 || sre.action.ID != 8 || sre.approver != 4 {
		t.Fatalf("sre runner %+v, want item 8 for approver 4 once", sre)
	}
}

func TestAddApprovedActionRunner_OverlapIsAStartupErrorNamingBoth(t *testing.T) {
	cases := map[string][2][]string{
		"same prefix":    {{"plan_step:"}, {"plan_step:"}},
		"longer second":  {{"sre_"}, {"sre_maintenance:"}},
		"shorter second": {{"sre_maintenance:"}, {"sre_"}},
		"one of many":    {{"a:", "plan_step:"}, {"b:", "plan_step:4"}},
	}
	for name, c := range cases {
		exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
		if err := exec.AddApprovedActionRunner("first", c[0], &fakeRunner{}); err != nil {
			t.Fatalf("%s: first add: %v", name, err)
		}
		err := exec.AddApprovedActionRunner("second", c[1], &fakeRunner{owns: true})
		if !errors.Is(err, ErrRunnerOverlap) || !strings.Contains(err.Error(), `"first"`) ||
			!strings.Contains(err.Error(), `"second"`) {
			t.Fatalf("%s: overlap = %v, want ErrRunnerOverlap naming both runners", name, err)
		}
		// The refused runner is not registered.
		if n := exec.approvedRunnerCount(); n != 1 {
			t.Fatalf("%s: %d runners registered after a refused add, want 1", name, n)
		}
	}
}

func TestAddApprovedActionRunner_RefusesInvalidRegistrations(t *testing.T) {
	cases := map[string]struct {
		name     string
		prefixes []string
		runner   ApprovedActionRunner
	}{
		"nil runner":   {"plans", []string{"plan_step:"}, nil},
		"no name":      {"", []string{"plan_step:"}, &fakeRunner{}},
		"no prefixes":  {"plans", nil, &fakeRunner{}},
		"empty prefix": {"plans", []string{""}, &fakeRunner{}},
		"blank prefix": {"plans", []string{"  "}, &fakeRunner{}},
	}
	for label, c := range cases {
		exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
		err := exec.AddApprovedActionRunner(c.name, c.prefixes, c.runner)
		if err == nil || !errors.Is(err, ErrInvalidRunner) {
			t.Fatalf("%s: add = %v, want ErrInvalidRunner", label, err)
		}
	}
}

// A plan step with no plan runner is refused: the manual path never runs
// it (fail closed), whatever approved it.
func TestRunApprovedAction_PlanStepWithoutItsRunnerIsRefused(t *testing.T) {
	for name, runner := range map[string]*fakeRunner{
		"no runner": nil, "runner declines": {owns: false},
	} {
		exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
		if runner != nil {
			if err := exec.AddApprovedActionRunner("plans",
				[]string{PlanStepIdentityPrefix}, runner); err != nil {
				t.Fatalf("%s: add: %v", name, err)
			}
		}
		_, err := exec.RunApprovedAction(context.Background(), queuedPlanStep(), 2)
		if !errors.Is(err, ErrPlanStepOutsidePlan) {
			t.Fatalf("%s: run = %v, want ErrPlanStepOutsidePlan", name, err)
		}
		if strings.Contains(err.Error(), "SQL validation") {
			t.Fatalf("%s: the manual path saw a plan step: %v", name, err)
		}
		if runner != nil && runner.calls != 0 {
			t.Fatalf("%s: a declining runner ran %d times", name, runner.calls)
		}
	}
}

// An item matching a runner's prefix that the runner does not own keeps
// the manual path (the SRE runner declines unknown proposals).
func TestRunApprovedAction_UnownedPrefixKeepsTheManualPath(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	runner := &fakeRunner{owns: false}
	if err := exec.AddApprovedActionRunner("sre", []string{"sre_proposal:"},
		runner); err != nil {
		t.Fatalf("add: %v", err)
	}
	item := queuedSRE()
	item.ProposedSQL = "DROP TABLE x"
	_, err := exec.RunApprovedAction(context.Background(), item, 2)
	if err == nil || !strings.Contains(err.Error(), "SQL validation") || runner.calls != 0 {
		t.Fatalf("run = %v (calls %d), want the manual path's validation", err, runner.calls)
	}
}

// An item whose key no runner's prefix matches never reaches a runner,
// even one whose Owns would say yes.
func TestRunApprovedAction_PrefixBoundsOwnership(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	greedy := &fakeRunner{owns: true}
	if err := exec.AddApprovedActionRunner("greedy", []string{"other:"}, greedy); err != nil {
		t.Fatalf("add: %v", err)
	}
	item := store.QueuedAction{ID: 6, FindingID: 3, ProposedSQL: "DROP TABLE x"}
	if _, err := exec.RunApprovedAction(context.Background(), item, 2); err == nil ||
		greedy.calls != 0 {
		t.Fatalf("run = %v, calls %d: an unmatched item reached a runner", err, greedy.calls)
	}
}

// Concurrent registration of disjoint runners and concurrent runs are
// race free and keep every registration.
func TestAddApprovedActionRunner_ConcurrentAddsAndRuns(t *testing.T) {
	exec := New(nil, advisoryConfig(), time.Time{}, nopLog)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- exec.AddApprovedActionRunner(fmt.Sprintf("r%d", i),
				[]string{fmt.Sprintf("k%d:", i)}, &fakeRunner{owns: true})
		}()
		go func() {
			defer wg.Done()
			_, _ = exec.RunApprovedAction(context.Background(), queuedSRE(), 1)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent add: %v", err)
		}
	}
	if n := exec.approvedRunnerCount(); n != 8 {
		t.Fatalf("%d runners registered, want 8", n)
	}
}
