package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

// No concurrent access tests: the orchestrator holds no state between
// Apply calls; each call's plan, result and record are its own.

const setNotNullSQL = `ALTER TABLE public.users ALTER COLUMN email SET NOT NULL`

func notNullRequest() Request {
	return Request{SQL: setNotNullSQL, Cycle: 2,
		Table: planpkg.TableFacts{Schema: "public", Name: "users"}}
}

// Happy path: an expanded SET NOT NULL returns both contract statements,
// a forward-fix script that asserts the proof, and the plan's delay.
func TestApplyReturnsContractScriptAndDelay(t *testing.T) {
	parsetree.Require(t)
	fixture := newFixture()
	fixture.rehearser.result.Measurement = passingRuntimeMeasurement()
	result, err := fixture.orchestrator().Apply(context.Background(), notNullRequest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.Verdict != VerdictExpanded || result.Reason != ReasonContractPending {
		t.Fatalf("result = %#v", result)
	}
	if len(result.PendingContractSQL) != 2 ||
		!strings.Contains(result.PendingContractSQL[0], "SET NOT NULL") ||
		!strings.Contains(result.PendingContractSQL[1], "DROP CONSTRAINT") {
		t.Fatalf("PendingContractSQL = %#v", result.PendingContractSQL)
	}
	if !strings.Contains(result.ContractScript, "users_email_nn") ||
		!strings.Contains(result.ContractScript, result.PendingContractSQL[0]) {
		t.Fatalf("ContractScript = %q", result.ContractScript)
	}
	if fixture.recorder.last.ContractDelay != time.Hour {
		t.Fatalf("recorded delay = %s, want 1h", fixture.recorder.last.ContractDelay)
	}
}

// A run that never expanded has no contract to wait for and no script.
func TestNonExpandedRunsRecordNoContractDelay(t *testing.T) {
	parsetree.Require(t)
	fixture := newFixture()
	fixture.rehearser.result = rehearsalpkg.Result{
		Verdict: rehearsalpkg.VerdictRecommendOnly,
		Reason:  rehearsalpkg.ReasonInconclusiveWorkload,
	}
	result, err := fixture.orchestrator().Apply(context.Background(), notNullRequest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.ContractScript != "" || len(result.PendingContractSQL) != 0 ||
		fixture.recorder.last.ContractDelay != 0 {
		t.Fatalf("result=%#v recorded delay=%s", result, fixture.recorder.last.ContractDelay)
	}
	if result.Reason != string(rehearsalpkg.ReasonInconclusiveWorkload) {
		t.Fatalf("Reason = %q", result.Reason)
	}
}

// Error propagation: a budget park carries the measured value in the
// reason, so the agent and the ledger both see why it parked.
func TestApplyStepBudgetParkCarriesMeasuredValue(t *testing.T) {
	parsetree.Require(t)
	fixture := newFixture()
	fixture.rehearser.result = rehearsalpkg.Result{
		Verdict: rehearsalpkg.VerdictPark, Reason: rehearsalpkg.ReasonStepBudget,
		Detail: "step validate_constraint ran 7m0s",
	}
	result, err := fixture.orchestrator().Apply(context.Background(), notNullRequest())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "step_budget_exceeded: step validate_constraint ran 7m0s"
	if result.Verdict != VerdictParked || result.Reason != want ||
		fixture.recorder.last.Reason != want || fixture.applier.calls != 0 {
		t.Fatalf("result=%#v recorded=%q applies=%d", result,
			fixture.recorder.last.Reason, fixture.applier.calls)
	}
}

// State transition (CHECK-25): contract_not_before is now + the delay,
// not now + (contract cycle - cycle) minutes.
func TestContractTimeIsNowPlusDelay(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	record := Record{Request: Request{Cycle: 3}, ContractDelay: time.Hour}
	got := contractTime(now, record)
	if got == nil || !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("contractTime = %v, want %v", got, now.Add(time.Hour))
	}
	for _, delay := range []time.Duration{0, -time.Minute} {
		if got := contractTime(now, Record{ContractDelay: delay}); got != nil {
			t.Fatalf("delay %s: contractTime = %v, want nil", delay, got)
		}
	}
}

// The measurement JSON names the slowest step and lists every step.
func TestMeasurementJSONListsSteps(t *testing.T) {
	raw, err := measurementJSON(Record{Reason: "contract_pending",
		Measurement: &rehearsalpkg.Measurement{
			MaxStepDuration: 1500 * time.Millisecond,
			Steps: []rehearsalpkg.StepMeasurement{{
				Kind: planpkg.StepAddCheckNotValid, Duration: 1500 * time.Millisecond,
				LockLevel: "ACCESS EXCLUSIVE",
			}, {
				Kind: planpkg.StepCreateUniqueIndex, Duration: time.Millisecond,
				Concurrent: true, LockLevel: "SHARE UPDATE EXCLUSIVE",
			}},
		}})
	if err != nil {
		t.Fatalf("measurementJSON: %v", err)
	}
	var doc struct {
		MaxStep float64 `json:"max_step_duration_ms"`
		Steps   []struct {
			Kind       string  `json:"kind"`
			DurationMS float64 `json:"duration_ms"`
			Concurrent bool    `json:"concurrent"`
			Lock       string  `json:"lock"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if doc.MaxStep != 1500 || len(doc.Steps) != 2 || doc.Steps[0].Kind != "add_check_not_valid" ||
		doc.Steps[0].DurationMS != 1500 || doc.Steps[0].Lock != "ACCESS EXCLUSIVE" ||
		!doc.Steps[1].Concurrent {
		t.Fatalf("measurement = %s", raw)
	}
}
