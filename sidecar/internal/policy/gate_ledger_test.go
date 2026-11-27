package policy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/ledger"
)

// policy-01: a request without a typed contract produced a decision with an
// empty risk tier, which sage.decision's CHECK rejects. The gate then
// answered policy_unavailable and the park never reached the ledger. A
// contract-less (or unknown-tier) decision now records as high, and a
// record failure is reported with the verdict the gate meant to record.

// ledgerRepo is a sage.decision stand-in behind the real ledger service,
// so every recorded decision passes ledger validation first.
type ledgerRepo struct {
	mu       sync.Mutex
	inserted []ledger.DecisionInput
}

func (r *ledgerRepo) InsertDecision(_ context.Context, in ledger.DecisionInput) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inserted = append(r.inserted, in)
	return int64(len(r.inserted)), nil
}

func (r *ledgerRepo) FindAuditViolations(context.Context) ([]ledger.AuditViolation, error) {
	return nil, nil
}

func (r *ledgerRepo) rows() []ledger.DecisionInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ledger.DecisionInput(nil), r.inserted...)
}

// ledgerRecorder maps a gate decision onto a ledger input as the executor
// does and records it through ledger.Service.
func ledgerRecorder(repo *ledgerRepo) func(
	context.Context, ActionRequest, Decision) (string, int64, error) {
	return func(ctx context.Context, req ActionRequest, d Decision) (string, int64, error) {
		verdict := ledger.Verdict(d.Verdict)
		if d.Verdict == VerdictPark {
			verdict = ledger.VerdictPark
		}
		recorded, err := ledger.NewService(repo).RecordDecision(ctx, ledger.DecisionInput{
			Feature: "truncate", Intent: "truncate", Verdict: verdict,
			Reason: string(d.Reason), RiskTier: string(d.RiskTier), PolicyVersion: 1,
			ProposedSQL: req.SQL, TargetObjects: req.TargetObjs,
		})
		return recorded.EvidenceID, recorded.ID, err
	}
}

func ledgerGate(runtime RuntimeState, runtimeErr error, repo *ledgerRepo) Gate {
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return runtime, runtimeErr
		},
		ValidateSQL: func(string) error { return nil },
		Policy: func(context.Context, ActionRequest) (Document, error) {
			doc := UnattendedProfile()
			doc.MaintenanceWindows = []string{"always"}
			return doc, nil
		},
		RecordDecisionDetailed: ledgerRecorder(repo),
	})
}

func ledgerRuntime() RuntimeState {
	return RuntimeState{ExecutorEnabled: true, TrustLevel: TrustAutonomous,
		ExecutionMode: ExecutionAuto}
}

func contractlessRequest() ActionRequest {
	return ActionRequest{Feature: "truncate", SQL: "TRUNCATE public.orders",
		TargetObjs: []string{"public.orders"}}
}

func TestAuthorizeRecordsContractlessParkAsHigh(t *testing.T) {
	repo := &ledgerRepo{}
	decision := ledgerGate(ledgerRuntime(), nil, repo).
		Authorize(context.Background(), contractlessRequest())

	if decision.Verdict != VerdictPark || decision.Reason != ReasonNoTypedContract {
		t.Fatalf("decision = %#v, want park/no_typed_contract (not policy_unavailable)",
			decision)
	}
	if decision.RiskTier != RiskHigh || decision.DecisionID != 1 || decision.EvidenceID == "" {
		t.Fatalf("decision = %#v, want a recorded high-tier decision", decision)
	}
	rows := repo.rows()
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(rows))
	}
	if rows[0].RiskTier != "high" || rows[0].Reason != "no_typed_contract" ||
		rows[0].Verdict != ledger.VerdictPark {
		t.Fatalf("recorded = %#v, want parked/no_typed_contract at tier high", rows[0])
	}
}

// Each early return runs before contract validation and used to record an
// empty tier as well.
func TestAuthorizeRecordsEveryContractlessEarlyReturnAsHigh(t *testing.T) {
	replica := ledgerRuntime()
	replica.IsReplica = true
	tests := []struct {
		name       string
		runtime    RuntimeState
		runtimeErr error
		want       Reason
	}{
		{"executor disabled", RuntimeState{}, nil, ReasonExecutorDisabled},
		{"emergency stop", RuntimeState{ExecutorEnabled: true, EmergencyStop: true}, nil,
			ReasonEmergencyStop},
		{"replica mutation", replica, nil, ReasonReplicaMutation},
		{"runtime error", RuntimeState{}, errors.New("runtime read failed"),
			ReasonPolicyUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &ledgerRepo{}
			decision := ledgerGate(tt.runtime, tt.runtimeErr, repo).
				Authorize(context.Background(), contractlessRequest())
			if decision.Verdict != VerdictBlocked || decision.Reason != tt.want ||
				decision.RiskTier != RiskHigh || decision.DecisionID == 0 {
				t.Fatalf("decision = %#v, want a recorded blocked/%s at high", decision, tt.want)
			}
			rows := repo.rows()
			if len(rows) != 1 || rows[0].RiskTier != "high" || rows[0].Reason != string(tt.want) {
				t.Fatalf("recorded = %#v, want one %s row at tier high", rows, tt.want)
			}
		})
	}
}

// A contract whose tier is missing or unknown is no better known than no
// contract: it records as high (unknown_classification: fail_closed).
func TestAuthorizeRecordsUnknownContractTierAsHigh(t *testing.T) {
	for _, tier := range []RiskTier{"", "bogus", "HIGH"} {
		t.Run(string(tier), func(t *testing.T) {
			repo := &ledgerRepo{}
			req := contractlessRequest()
			req.Contract = &ActionContract{ActionType: "truncate_table", RiskTier: tier}
			decision := ledgerGate(ledgerRuntime(), nil, repo).
				Authorize(context.Background(), req)
			if decision.Verdict == VerdictExecute || decision.Reason == ReasonPolicyUnavailable {
				t.Fatalf("decision = %#v, an unknown tier must be withheld and recorded",
					decision)
			}
			rows := repo.rows()
			if decision.RiskTier != RiskHigh || len(rows) != 1 || rows[0].RiskTier != "high" {
				t.Fatalf("decision=%#v rows=%#v, want tier high", decision, rows)
			}
		})
	}
}

// Boundary: the four known tiers are recorded exactly as the contract says.
func TestAuthorizeKeepsEveryKnownContractTier(t *testing.T) {
	for _, tier := range []RiskTier{RiskReadOnly, RiskSafe, RiskModerate, RiskHigh} {
		t.Run(string(tier), func(t *testing.T) {
			repo := &ledgerRepo{}
			req := contractlessRequest()
			req.Contract = &ActionContract{ActionType: "truncate_table", RiskTier: tier}
			runtime := RuntimeState{ExecutorEnabled: true, EmergencyStop: true}
			decision := ledgerGate(runtime, nil, repo).Authorize(context.Background(), req)
			rows := repo.rows()
			if decision.RiskTier != tier || len(rows) != 1 || rows[0].RiskTier != string(tier) {
				t.Fatalf("decision=%#v rows=%#v, want tier %s", decision, rows, tier)
			}
		})
	}
}

func TestExplainReportsContractlessAsHighWithoutRecording(t *testing.T) {
	repo := &ledgerRepo{}
	gate := ledgerGate(ledgerRuntime(), nil, repo).(Explainer)
	decision := gate.Explain(context.Background(), contractlessRequest())
	if decision.Verdict != VerdictPark || decision.Reason != ReasonNoTypedContract ||
		decision.RiskTier != RiskHigh {
		t.Fatalf("explain = %#v, want park/no_typed_contract at high", decision)
	}
	if rows := repo.rows(); len(rows) != 0 {
		t.Fatalf("Explain recorded %d rows, want 0", len(rows))
	}
}

// Concurrent contract-less authorizations each record their own high row.
func TestAuthorizeContractlessConcurrently(t *testing.T) {
	const workers = 16
	repo := &ledgerRepo{}
	gate := ledgerGate(ledgerRuntime(), nil, repo)
	var wg sync.WaitGroup
	results := make(chan Decision, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- gate.Authorize(context.Background(), contractlessRequest())
		}()
	}
	wg.Wait()
	close(results)
	for decision := range results {
		if decision.Verdict != VerdictPark || decision.RiskTier != RiskHigh {
			t.Fatalf("concurrent decision = %#v, want park at high", decision)
		}
	}
	rows := repo.rows()
	if len(rows) != workers {
		t.Fatalf("recorded %d rows, want %d", len(rows), workers)
	}
	for _, row := range rows {
		if row.RiskTier != "high" {
			t.Fatalf("row = %#v, want tier high", row)
		}
	}
}

// recordFailure is one RecordFailed report.
type recordFailure struct {
	decision Decision
	err      error
}

type failureSink struct {
	mu      sync.Mutex
	reports []recordFailure
}

func (s *failureSink) hook(_ context.Context, _ ActionRequest, d Decision, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, recordFailure{decision: d, err: err})
}

func (s *failureSink) all() []recordFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordFailure(nil), s.reports...)
}

func failingRecorderGate(recordErr error, sink *failureSink) Gate {
	config := GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return ledgerRuntime(), nil
		},
		ValidateSQL: func(string) error { return nil },
		RecordDecisionDetailed: func(context.Context, ActionRequest, Decision) (
			string, int64, error) {
			return "", 0, recordErr
		},
	}
	if sink != nil {
		config.RecordFailed = sink.hook
	}
	return NewGate(config)
}

// A failed record still fails closed (policy_unavailable), but the detail
// names the verdict the gate meant to record and the hook hears it once.
func TestRecordFailureNamesOriginalVerdictAndReportsIt(t *testing.T) {
	recordErr := errors.New(`violates check constraint "decision_risk_tier_check"`)
	sink := &failureSink{}
	decision := failingRecorderGate(recordErr, sink).
		Authorize(context.Background(), contractlessRequest())

	if decision.Verdict != VerdictBlocked || decision.Reason != ReasonPolicyUnavailable {
		t.Fatalf("decision = %#v, want blocked/policy_unavailable (fail closed)", decision)
	}
	if !strings.Contains(decision.Detail, "parked/no_typed_contract") ||
		!strings.Contains(decision.Detail, "decision_risk_tier_check") {
		t.Fatalf("detail = %q, want the original verdict, reason and error", decision.Detail)
	}
	if decision.RiskTier != RiskHigh {
		t.Fatalf("risk tier = %q, want high", decision.RiskTier)
	}
	reports := sink.all()
	if len(reports) != 1 {
		t.Fatalf("RecordFailed calls = %d, want 1", len(reports))
	}
	got := reports[0]
	if got.decision.Verdict != VerdictPark || got.decision.Reason != ReasonNoTypedContract ||
		got.decision.RiskTier != RiskHigh || !errors.Is(got.err, recordErr) {
		t.Fatalf("report = %#v, want the parked decision and the record error", got)
	}
}

// Nil hook: the failure still fails closed with a descriptive detail.
func TestRecordFailureWithoutHookFailsClosed(t *testing.T) {
	decision := failingRecorderGate(errors.New("connection refused"), nil).
		Authorize(context.Background(), contractlessRequest())
	if decision.Verdict != VerdictBlocked || decision.Reason != ReasonPolicyUnavailable ||
		!strings.Contains(decision.Detail, "parked/no_typed_contract") ||
		!strings.Contains(decision.Detail, "connection refused") {
		t.Fatalf("decision = %#v", decision)
	}
}

// The plain RecordDecision recorder reports failures the same way.
func TestRecordFailureOfPlainRecorderIsReported(t *testing.T) {
	sink := &failureSink{}
	gate := NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return RuntimeState{}, nil
		},
		RecordDecision: func(context.Context, ActionRequest, Decision) (string, error) {
			return "", errors.New("disk full")
		},
		RecordFailed: sink.hook,
	})
	decision := gate.Authorize(context.Background(), contractlessRequest())
	reports := sink.all()
	if decision.Reason != ReasonPolicyUnavailable ||
		!strings.Contains(decision.Detail, "blocked/executor_disabled") ||
		len(reports) != 1 || reports[0].decision.Reason != ReasonExecutorDisabled {
		t.Fatalf("decision=%#v reports=%#v", decision, reports)
	}
}

// recordLockGate runs budget-spending requests under a Serialize stub that
// records how the transaction ended.
func recordLockGate(recordErr, commitErr error, sink *failureSink, ended *[]bool) Gate {
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return ledgerRuntime(), nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy: func(context.Context, ActionRequest) (Document, error) {
			doc := UnattendedProfile()
			doc.MaintenanceWindows = []string{"always"}
			return doc, nil
		},
		Serialize: func(ctx context.Context, _ ActionRequest) (
			context.Context, func(bool) error, error) {
			return ctx, func(commit bool) error {
				*ended = append(*ended, commit)
				if commit {
					return commitErr
				}
				return nil
			}, nil
		},
		RecordDecisionDetailed: func(context.Context, ActionRequest, Decision) (
			string, int64, error) {
			return "ev_1", 1, recordErr
		},
		RecordFailed: sink.hook,
	})
}

func safeRequest() ActionRequest {
	req := contractlessRequest()
	req.Feature, req.SQL = "analyze", "ANALYZE public.orders"
	req.Contract = &ActionContract{ActionType: "analyze_table", RiskTier: RiskSafe}
	return req
}

func TestSerializedRecordFailureRollsBackAndReports(t *testing.T) {
	sink, ended := &failureSink{}, []bool{}
	decision := recordLockGate(errors.New("insert failed"), nil, sink, &ended).
		Authorize(context.Background(), safeRequest())
	reports := sink.all()
	if decision.Verdict != VerdictBlocked || decision.Reason != ReasonPolicyUnavailable ||
		!strings.Contains(decision.Detail, "blocked/trust_ramp_not_satisfied") ||
		!strings.Contains(decision.Detail, "insert failed") {
		t.Fatalf("decision = %#v, want policy_unavailable naming the original", decision)
	}
	if len(ended) != 1 || ended[0] {
		t.Fatalf("transaction ends = %v, want one rollback", ended)
	}
	if len(reports) != 1 || reports[0].decision.Reason != ReasonTrustRampNotSatisfied ||
		reports[0].decision.RiskTier != RiskSafe {
		t.Fatalf("reports = %#v, want the trust-ramp decision once", reports)
	}
}

// A commit that fails loses the recorded row: it is a record failure too.
func TestSerializedCommitFailureReports(t *testing.T) {
	sink, ended := &failureSink{}, []bool{}
	decision := recordLockGate(nil, errors.New("commit lost"), sink, &ended).
		Authorize(context.Background(), safeRequest())
	reports := sink.all()
	if decision.Reason != ReasonPolicyUnavailable || decision.RiskTier != RiskSafe ||
		!strings.Contains(decision.Detail, "blocked/trust_ramp_not_satisfied") ||
		!strings.Contains(decision.Detail, "commit lost") {
		t.Fatalf("decision = %#v", decision)
	}
	if len(ended) != 1 || !ended[0] || len(reports) != 1 ||
		reports[0].decision.Reason != ReasonTrustRampNotSatisfied {
		t.Fatalf("ends=%v reports=%#v, want one commit and one report", ended, reports)
	}
}

// A successful serialized record reports nothing.
func TestSerializedSuccessReportsNothing(t *testing.T) {
	sink, ended := &failureSink{}, []bool{}
	decision := recordLockGate(nil, nil, sink, &ended).
		Authorize(context.Background(), safeRequest())
	if decision.Reason != ReasonTrustRampNotSatisfied || decision.DecisionID != 1 ||
		len(sink.all()) != 0 || len(ended) != 1 || !ended[0] {
		t.Fatalf("decision=%#v reports=%#v ends=%v", decision, sink.all(), ended)
	}
}
