package policy

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// usageByRequest answers usage per (kind, targets) and counts each read.
type usageByRequest struct {
	mu    sync.Mutex
	usage map[string]LimitUsage
	reads map[string]int
}

func usageKeyForTest(req ActionRequest) string {
	return string(BudgetKindFor(req)) + "|" + strings.Join(req.TargetObjs, ",")
}

func (u *usageByRequest) read(_ context.Context, req ActionRequest) (LimitUsage, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.reads == nil {
		u.reads = map[string]int{}
	}
	u.reads[usageKeyForTest(req)]++
	return u.usage[usageKeyForTest(req)], nil
}

func perTargetGate(t *testing.T, u *usageByRequest) BatchExplainer {
	t.Helper()
	fx := &budgetFixture{doc: budgetDoc()}
	gate := newBudgetGate(t, fx).(*authorizationGate)
	gate.config.Usage = u.read
	return gate
}

// CHECK-08: each (kind, targets, SQL) reads its own usage, so a table at
// its budget is not explained with another table's usage. The same
// statement on the same table reads once.
func TestExplainBatchReadsUsagePerTarget(t *testing.T) {
	u := &usageByRequest{usage: map[string]LimitUsage{
		"performance|public.a": {TablesInWindow: 9},
		"performance|public.b": {TablesInWindow: 1},
	}}
	got := perTargetGate(t, u).ExplainBatch(context.Background(), []ActionRequest{
		perfRequest("public.a"), perfRequest("public.b"), perfRequest("public.a"),
		hygieneRequest("public.i"),
	})
	if got[0].Verdict != VerdictPark || got[1].Verdict != VerdictExecute ||
		got[2].Verdict != VerdictPark || got[3].Verdict != VerdictExecute {
		t.Fatalf("batch = %v/%v/%v/%v, want park, execute, park, execute",
			got[0].Verdict, got[1].Verdict, got[2].Verdict, got[3].Verdict)
	}
	want := map[string]int{"performance|public.a": 1, "performance|public.b": 1,
		"hygiene|public.i": 1}
	if len(u.reads) != len(want) {
		t.Fatalf("usage reads = %v, want %v", u.reads, want)
	}
	for key, n := range want {
		if u.reads[key] != n {
			t.Fatalf("usage reads = %v, want %v", u.reads, want)
		}
	}
}

// Two statements on one table are different usage questions: each counts
// its own rows rewritten.
func TestExplainBatchUsageKeyIncludesSQL(t *testing.T) {
	u := &usageByRequest{}
	other := perfRequest("public.a")
	other.SQL = "CREATE INDEX CONCURRENTLY idx_other ON public.a (b)"
	perTargetGate(t, u).ExplainBatch(context.Background(),
		[]ActionRequest{perfRequest("public.a"), other})
	if u.reads["performance|public.a"] != 2 {
		t.Fatalf("usage reads = %v, want two reads for two statements", u.reads)
	}
}

// Family requests (no SQL, no target) still share one read per kind
// (cases-06).
func TestExplainBatchFamiliesShareUsagePerKind(t *testing.T) {
	u := &usageByRequest{}
	first, second := perfRequest(""), perfRequest("")
	for _, req := range []*ActionRequest{&first, &second} {
		req.SQL, req.TargetObjs, req.ExplainFamily = "", nil, true
	}
	perTargetGate(t, u).ExplainBatch(context.Background(), []ActionRequest{first, second})
	if u.reads["performance|"] != 1 {
		t.Fatalf("family usage reads = %v, want one", u.reads)
	}
}

// A nil usage reader stays nil, so the gate's own handling applies.
func TestMemoizeUsageKeepsNilReader(t *testing.T) {
	if memoizeUsageByRequest(nil) != nil {
		t.Fatal("nil usage reader became non-nil")
	}
}
