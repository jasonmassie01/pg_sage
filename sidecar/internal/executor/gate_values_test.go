package executor

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// ux-05b: the dashboard labels every verdict and reason the policy gate
// can serve (Actions > Held back, a case's "Why hasn't this run?"), with
// no raw enum left on screen. gateValues.json is the generated list its
// label-coverage test (web/src/pages/actions/workflow.test.js) reads; this
// test fails when the list drifts from the Go constants, so a new reason
// constant fails here until the list is regenerated, and then fails the
// web test until it has a label. Regenerate with
// SAGE_UPDATE_GATE_VALUES=1 go test ./internal/executor -run TestGateValues.
// No concurrent access tests: it reads constants and one file.

const gateValuesPath = "../../web/src/pages/actions/gateValues.json"

type gateValues struct {
	Source   string   `json:"source"`
	Verdicts []string `json:"verdicts"`
	Reasons  []string `json:"reasons"`
}

// wantGateValues is every verdict spelling a dashboard surface reads (the
// gate's, the ledger's "parked" and the executor's "queue_for_approval")
// and every reason a decision can be recorded with.
func wantGateValues() gateValues {
	verdicts := map[string]bool{}
	for _, v := range []string{
		string(policy.VerdictExecute), string(policy.VerdictQueueApproval),
		string(policy.VerdictPark), string(policy.VerdictBlocked),
		string(policy.VerdictObserveOnly),
		string(ledger.VerdictExecute), string(ledger.VerdictQueueApproval),
		string(ledger.VerdictPark), string(ledger.VerdictBlocked),
		string(ledger.VerdictObserveOnly),
		PolicyDecisionExecute, PolicyDecisionQueueApproval, PolicyDecisionBlocked,
		PolicyDecisionObserveOnly, PolicyDecisionParked,
	} {
		verdicts[v] = true
	}
	out := gateValues{Source: "internal/executor/gate_values_test.go; do not edit"}
	for v := range verdicts {
		out.Verdicts = append(out.Verdicts, v)
	}
	for _, r := range policy.LedgerReasons() {
		out.Reasons = append(out.Reasons, string(r))
	}
	sort.Strings(out.Verdicts)
	sort.Strings(out.Reasons)
	return out
}

func TestGateValues_MatchTheDashboardList(t *testing.T) {
	want := wantGateValues()
	if os.Getenv("SAGE_UPDATE_GATE_VALUES") == "1" {
		body, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatalf("encode gate values: %v", err)
		}
		if err := os.WriteFile(gateValuesPath, append(body, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", gateValuesPath, err)
		}
	}
	raw, err := os.ReadFile(gateValuesPath)
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s is missing; regenerate it with SAGE_UPDATE_GATE_VALUES=1", gateValuesPath)
	}
	if err != nil {
		t.Fatalf("read %s: %v", gateValuesPath, err)
	}
	var got gateValues
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse %s: %v", gateValuesPath, err)
	}
	if !reflect.DeepEqual(got.Verdicts, want.Verdicts) {
		t.Errorf("dashboard verdicts = %v, the gate serves %v; regenerate with "+
			"SAGE_UPDATE_GATE_VALUES=1", got.Verdicts, want.Verdicts)
	}
	if !reflect.DeepEqual(got.Reasons, want.Reasons) {
		t.Errorf("dashboard reasons drifted from policy.LedgerReasons (%d listed, %d "+
			"declared); regenerate with SAGE_UPDATE_GATE_VALUES=1", len(got.Reasons),
			len(want.Reasons))
	}
}

// The list carries the values the labels must cover, not a subset: the
// five held-back reasons ux-05b names and both spellings of a park and of
// an approval queue.
func TestGateValues_IncludeTheHeldBackSpellings(t *testing.T) {
	want := wantGateValues()
	has := func(list []string, v string) bool {
		i := sort.SearchStrings(list, v)
		return i < len(list) && list[i] == v
	}
	for _, v := range []string{"park", "parked", "queue_approval", "queue_for_approval",
		"execute", "blocked", "observe_only"} {
		if !has(want.Verdicts, v) {
			t.Errorf("verdict %q is missing from the generated list", v)
		}
	}
	for _, r := range []string{"budget_exceeded", "awaiting_verification", "emergency_stop",
		"replica_mutation", "custodian_backoff", "agent_frozen"} {
		if !has(want.Reasons, r) {
			t.Errorf("reason %q is missing from the generated list", r)
		}
	}
	if len(want.Verdicts) != 7 {
		t.Errorf("generated %d verdict spellings, want 7: %v", len(want.Verdicts), want.Verdicts)
	}
}
