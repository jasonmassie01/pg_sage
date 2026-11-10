package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// llm-03 part 1: budgets.llm_tokens_daily from the standing policy becomes
// a client ceiling, read through a 60 s cache.

type fakePolicyReads struct {
	calls  int
	policy policy.Policy
	err    error
}

func (f *fakePolicyReads) read(context.Context) (policy.Policy, error) {
	f.calls++
	return f.policy, f.err
}

func policyWithTokens(t *testing.T, tokens policy.BudgetLimit) policy.Policy {
	t.Helper()
	doc := policy.UnattendedProfile()
	doc.Budgets.LLMTokensDaily = tokens
	raw, err := policy.MarshalDocument(doc)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	return policy.Policy{Version: 2, Document: json.RawMessage(raw)}
}

type ceilingHarness struct {
	reads    *fakePolicyReads
	now      time.Time
	warnings []string
	ceiling  *policyCeiling
}

func newCeilingHarness(reads *fakePolicyReads) *ceilingHarness {
	h := &ceilingHarness{reads: reads, now: time.Unix(1_800_000_000, 0)}
	h.ceiling = &policyCeiling{
		label: "orders",
		read:  reads.read,
		now:   func() time.Time { return h.now },
		warn: func(msg string, args ...any) {
			h.warnings = append(h.warnings, fmt.Sprintf(msg, args...))
		},
	}
	return h
}

func TestPolicyCeiling_ReadsValueAndCaches(t *testing.T) {
	reads := &fakePolicyReads{policy: policyWithTokens(t, policy.NewBudgetLimit(100))}
	h := newCeilingHarness(reads)
	if v, ok := h.ceiling.Ceiling(); !ok || v != 100 {
		t.Fatalf("ceiling = %d, %t; want 100, true", v, ok)
	}
	h.now = h.now.Add(59 * time.Second)
	reads.policy = policyWithTokens(t, policy.NewBudgetLimit(5))
	if v, _ := h.ceiling.Ceiling(); v != 100 || reads.calls != 1 {
		t.Fatalf("within 60 s: ceiling %d after %d reads; want cached 100, 1 read",
			v, reads.calls)
	}
	h.now = h.now.Add(time.Second)
	if v, _ := h.ceiling.Ceiling(); v != 5 || reads.calls != 2 {
		t.Fatalf("at 60 s: ceiling %d after %d reads; want 5, 2 reads", v, reads.calls)
	}
}

func TestPolicyCeiling_NoCapAndMissingPolicyMeanConfigOnly(t *testing.T) {
	reads := &fakePolicyReads{policy: policyWithTokens(t, policy.NoCapBudget())}
	h := newCeilingHarness(reads)
	if v, ok := h.ceiling.Ceiling(); ok {
		t.Fatalf("NoCap ceiling = %d, true; want absent", v)
	}
	reads.err = policy.ErrNotFound
	h.now = h.now.Add(time.Minute)
	if v, ok := h.ceiling.Ceiling(); ok {
		t.Fatalf("no policy: ceiling = %d, true; want absent", v)
	}
	if len(h.warnings) != 0 {
		t.Fatalf("a missing policy is not an error: %v", h.warnings)
	}
}

func TestPolicyCeiling_ZeroIsACap(t *testing.T) {
	reads := &fakePolicyReads{policy: policyWithTokens(t, policy.NewBudgetLimit(0))}
	h := newCeilingHarness(reads)
	if v, ok := h.ceiling.Ceiling(); !ok || v != 0 {
		t.Fatalf("ceiling = %d, %t; want 0, true (no tokens)", v, ok)
	}
}

func TestPolicyCeiling_StoreErrorKeepsLastValueAndWarns(t *testing.T) {
	reads := &fakePolicyReads{policy: policyWithTokens(t, policy.NewBudgetLimit(100))}
	h := newCeilingHarness(reads)
	h.ceiling.Ceiling()
	reads.err = errors.New("read current policy: connection refused")
	h.now = h.now.Add(time.Minute)
	if v, ok := h.ceiling.Ceiling(); !ok || v != 100 {
		t.Fatalf("after a store error: ceiling = %d, %t; want last 100", v, ok)
	}
	if len(h.warnings) != 1 || !strings.Contains(h.warnings[0], "connection refused") ||
		!strings.Contains(h.warnings[0], "orders") ||
		!strings.Contains(h.warnings[0], "100") {
		t.Fatalf("warnings = %q, want one naming the database, cause and kept value",
			h.warnings)
	}
	// The failure is cached too: no read storm while the store is down.
	h.ceiling.Ceiling()
	if reads.calls != 2 {
		t.Fatalf("reads = %d, want 2 (failure cached for the TTL)", reads.calls)
	}
}

func TestPolicyCeiling_ErrorBeforeFirstReadIsConfigOnly(t *testing.T) {
	reads := &fakePolicyReads{err: policy.ErrUnavailable}
	h := newCeilingHarness(reads)
	if v, ok := h.ceiling.Ceiling(); ok {
		t.Fatalf("ceiling = %d, true; want absent with no value read yet", v)
	}
	if len(h.warnings) != 1 {
		t.Fatalf("warnings = %q, want one", h.warnings)
	}
}

func TestPolicyCeiling_UnparseableDocumentKeepsLastValue(t *testing.T) {
	reads := &fakePolicyReads{policy: policyWithTokens(t, policy.NewBudgetLimit(100))}
	h := newCeilingHarness(reads)
	h.ceiling.Ceiling()
	reads.policy = policy.Policy{Version: 3, Document: json.RawMessage(`{"budgets":`)}
	h.now = h.now.Add(time.Minute)
	if v, ok := h.ceiling.Ceiling(); !ok || v != 100 {
		t.Fatalf("ceiling = %d, %t; want last 100", v, ok)
	}
	if len(h.warnings) != 1 {
		t.Fatalf("warnings = %q, want one", h.warnings)
	}
}

// Meta-DB mode scopes the ceiling to the database; otherwise global.
func TestPolicyCeiling_ScopeFollowsDatabaseID(t *testing.T) {
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Name: "orders", DatabaseID: 7}}
	if _, scope := rt.policyStoreScope(); scope.DatabaseID == nil || *scope.DatabaseID != 7 {
		t.Fatalf("meta-DB scope = %+v, want database 7", scope)
	}
	rt.spec.DatabaseID = 0
	if _, scope := rt.policyStoreScope(); scope.DatabaseID != nil {
		t.Fatalf("YAML/standalone scope = %+v, want global", scope)
	}
}

// Concurrent admissions share one read per TTL.
func TestPolicyCeiling_ConcurrentCallersReadOnce(t *testing.T) {
	reads := &fakePolicyReads{policy: policyWithTokens(t, policy.NewBudgetLimit(100))}
	h := newCeilingHarness(reads)
	done := make(chan int64, 20)
	for i := 0; i < 20; i++ {
		go func() {
			v, _ := h.ceiling.Ceiling()
			done <- v
		}()
	}
	for i := 0; i < 20; i++ {
		if v := <-done; v != 100 {
			t.Fatalf("ceiling = %d, want 100", v)
		}
	}
	if reads.calls != 1 {
		t.Fatalf("reads = %d, want 1 for 20 concurrent callers", reads.calls)
	}
}
