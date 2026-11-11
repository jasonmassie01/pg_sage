package policy

import (
	"sort"
	"sync"
	"testing"
)

// policy-07: GET /api/v1/decisions validates its reason filter against
// LedgerReasons. The list must be exactly the reasons a decision can be
// recorded with: the gate's own (this package), the agent decider's and
// the core agent reasons D1 and D2 record. The constants are read from
// source (doc_coverage_test.go's collector), so a new reason cannot ship
// without being filterable, and a removed one cannot linger.

// executorLedgerReasons are the reasons the executor records itself,
// outside the gate. The executor imports this package, so its test
// (custodian_escalation_reason_test.go) pins each one to its constant.
var executorLedgerReasons = []string{"custodian_backoff"}

func TestLedgerReasonsAreEveryRecordedReason(t *testing.T) {
	gate, err := collectPolicyConsts(".")
	if err != nil {
		t.Fatalf("collect policy constants: %v", err)
	}
	decide, err := collectPolicyConsts(agentDecideDir)
	if err != nil {
		t.Fatalf("collect agent decider constants: %v", err)
	}
	want := map[string]bool{}
	for _, reason := range append(append(append(gate.Reasons, decide.Reasons...),
		agentAccessReasons...), executorLedgerReasons...) {
		want[reason] = true
	}
	got := map[string]bool{}
	for _, reason := range LedgerReasons() {
		if got[string(reason)] {
			t.Errorf("LedgerReasons lists %q twice", reason)
		}
		got[string(reason)] = true
	}
	for reason := range want {
		if !got[reason] {
			t.Errorf("LedgerReasons misses recorded reason %q", reason)
		}
	}
	for reason := range got {
		if !want[reason] {
			t.Errorf("LedgerReasons lists %q, which no package declares", reason)
		}
	}
	if len(got) < minDocumentedReasons+minAgentDecideReasons {
		t.Fatalf("LedgerReasons has %d reasons, want at least %d", len(got),
			minDocumentedReasons+minAgentDecideReasons)
	}
}

func TestIsLedgerReason(t *testing.T) {
	cases := map[string]bool{
		"budget_exceeded":            true,
		"outside_maintenance_window": true,
		"agent_no_pitr":              true,
		"agent_frozen":               true,
		"ddl_conflict":               true,
		"custodian_backoff":          true,
		"bar":                        false,
		"":                           false,
		"Budget_Exceeded":            false,
		"budget_exceeded ":           false,
		"budget exceeded":            false,
		"grantor_lacks_privilege":    false, // refuses grants; never reaches the gate
	}
	for reason, want := range cases {
		if got := IsLedgerReason(reason); got != want {
			t.Errorf("IsLedgerReason(%q) = %v, want %v", reason, got, want)
		}
	}
}

// The returned slice is a copy: a caller that sorts or edits it cannot
// change what the next caller validates against.
func TestLedgerReasonsReturnsACopy(t *testing.T) {
	first := LedgerReasons()
	first[0] = "tampered"
	sort.Slice(first, func(i, j int) bool { return first[i] > first[j] })
	if IsLedgerReason("tampered") {
		t.Fatal("editing the returned slice changed the validated set")
	}
	if LedgerReasons()[0] == "tampered" {
		t.Fatal("LedgerReasons returned the shared slice")
	}
}

// Concurrent access: the API validates filters from many requests at once.
func TestIsLedgerReasonConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !IsLedgerReason("rate_limit_exceeded") || IsLedgerReason("foo") {
				errs <- "inconsistent answer under concurrency"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}
