package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	migrationruntime "github.com/pg-sage/sidecar/internal/migration/runtime"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: the outcome is built from one runtime
// result per call; the executor shares no state between calls.

const outcomeNotNullArgs = `{"table":"public.users","cycle":7,
	"sql":"ALTER TABLE public.users ALTER COLUMN email SET NOT NULL"}`

// runOutcome sends apply_migration through the real rehearsal runtime
// with a rehearser that returns rehearsal and an executing gate.
func runOutcome(t *testing.T, rehearsal rehearsalpkg.Result) MigrationOutcome {
	t.Helper()
	gate := &recordingProductionGate{decision: policy.Decision{
		Verdict: policy.VerdictExecute, EvidenceID: "ev-mcp-06"}}
	runtime := migrationruntime.NewOrchestrator(plan.NewPlanner(),
		fixedRehearser{rehearsal}, gate, noopApplier{}, noopRecorder{})
	executor := NewProductionIntentExecutor(&recordingIntentStore{}, plan.NewPlanner(), gate).
		WithMigrationRuntime(runtime)
	result, err := executor.ExecuteConcrete(context.Background(), policy.ActionRequest{
		DatabaseID: int64ProductionPointer(42),
		Contract:   &policy.ActionContract{ActionType: "apply_migration"},
		Arguments:  json.RawMessage(outcomeNotNullArgs),
	})
	require.NoError(t, err)
	return result.(MigrationOutcome)
}

// passedRehearsal promoted on one measured statement, on a receipted
// clone (lab-04b).
func passedRehearsal() rehearsalpkg.Result {
	blocks := int64(4)
	return rehearsalpkg.Result{Verdict: rehearsalpkg.VerdictPromoteExpand,
		Reason: rehearsalpkg.ReasonRehearsalPassed, CloneID: "clone-9", ReceiptID: 31,
		Provider: "dle", At: time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC),
		Measurement: rehearsalpkg.Measurement{AffectedQueries: []rehearsalpkg.QueryMeasurement{{
			QueryID: 9007199254740993, BeforeLatencyMS: 0.25, AfterLatencyMS: 0.3,
			BeforePlanHash: "v2u:1a", AfterPlanHash: "v2u:1a", BeforeCost: 8.3,
			AfterCost: 8.3, BeforeBlocks: &blocks, AfterBlocks: &blocks,
		}}}}
}

// Happy path: SET NOT NULL reports contract_pending, both contract
// statements and a forward-fix script whose header names the proof.
func TestApplyMigrationOutcomeCarriesPendingContract(t *testing.T) {
	parsetree.Require(t)
	outcome := runOutcome(t, passedRehearsal())
	require.Equal(t, "expanded", outcome.Verdict)
	require.Equal(t, "contract_pending", outcome.Reason)
	require.Equal(t, 2, len(outcome.PendingContractSQL))
	require.True(t, strings.Contains(outcome.PendingContractSQL[0], "SET NOT NULL"))
	require.True(t, strings.Contains(outcome.PendingContractSQL[1], "DROP CONSTRAINT"))
	header := outcome.ContractScript[:strings.Index(outcome.ContractScript, "SET LOCAL")]
	require.True(t, strings.Contains(header, "'users_email_nn'"))
	require.True(t, strings.Contains(header, "convalidated"))
}

// CHECK-27: the JSON golden for the apply_migration outcome includes
// reason and pending_contract_sql (the plan itself is pinned elsewhere).
// lab-04b CHECK-26: and the measured affected_queries and the rehearsal's
// tier-2 ladder record with its clone receipt.
func TestApplyMigrationOutcomeJSONGolden(t *testing.T) {
	parsetree.Require(t)
	raw, err := json.Marshal(runOutcome(t, passedRehearsal()))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.True(t, doc["plan"] != nil)
	require.True(t, strings.HasPrefix(doc["contract_script"].(string), "-- pg_sage"))
	delete(doc, "plan")
	doc["contract_script"] = "SCRIPT"
	got, err := json.Marshal(doc)
	require.NoError(t, err)
	const golden = `{"affected_queries":[{"after_blocks":4,"after_cost":8.3,` +
		`"after_latency_ms":0.3,"after_plan_hash":"v2u:1a","before_blocks":4,` +
		`"before_cost":8.3,"before_latency_ms":0.25,"before_plan_hash":"v2u:1a",` +
		`"query_id":"9007199254740993"}],` +
		`"contract_not_before_cycle":8,"contract_script":"SCRIPT",` +
		`"evidence_id":"ev-mcp-06","ladder":{"at":"2026-11-16T09:30:00Z",` +
		`"evidence":{"measured":1,"provider":"dle","reason":"rehearsal_passed",` +
		`"receipt_id":31,"statements":1,"verdict":"promote_expand"},` +
		`"method":"clone_rehearsal","name":"clone","result":"passed","tier":2},` +
		`"pending_contract_sql":[` +
		`"ALTER TABLE \"public\".\"users\" ALTER COLUMN \"email\" SET NOT NULL",` +
		`"ALTER TABLE \"public\".\"users\" DROP CONSTRAINT \"users_email_nn\""],` +
		`"reason":"contract_pending","verdict":"expanded"}`
	require.Equal(t, golden, string(got))
}

// Nil/empty: no workload evidence reaches the agent as a reason, not "".
func TestApplyMigrationOutcomeInconclusiveReason(t *testing.T) {
	parsetree.Require(t)
	outcome := runOutcome(t, rehearsalpkg.Result{Verdict: rehearsalpkg.VerdictRecommendOnly,
		Reason: rehearsalpkg.ReasonInconclusiveWorkload})
	require.Equal(t, "recommend_only", outcome.Verdict)
	require.Equal(t, "inconclusive_no_workload", outcome.Reason)
	require.Equal(t, 0, len(outcome.PendingContractSQL))
	require.Equal(t, "", outcome.ContractScript)
	raw, err := json.Marshal(outcome)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(raw), `"reason":"inconclusive_no_workload"`))
	require.False(t, strings.Contains(string(raw), "pending_contract_sql"))
}

// A clone-cleanup failure's text reaches Reason.
func TestApplyMigrationOutcomeCarriesCleanupFailure(t *testing.T) {
	parsetree.Require(t)
	rehearsal := passedRehearsal()
	rehearsal.CleanupError = "destroy rehearsal clone clone-9: 503 busy"
	outcome := runOutcome(t, rehearsal)
	require.Equal(t, "recommend_only", outcome.Verdict)
	require.True(t, strings.Contains(outcome.Reason, "clone_cleanup_failed"))
	require.True(t, strings.Contains(outcome.Reason, "clone-9: 503 busy"))
}

// Every apply_migration path sets a reason, the runtime-less one too.
func TestApplyMigrationOutcomeReasonWithoutRuntime(t *testing.T) {
	parsetree.Require(t)
	executor := NewProductionIntentExecutor(&recordingIntentStore{}, plan.NewPlanner(),
		&recordingProductionGate{decision: policy.Decision{Verdict: policy.VerdictExecute}})
	result, err := executor.ExecuteConcrete(context.Background(), policy.ActionRequest{
		Contract:  &policy.ActionContract{ActionType: "apply_migration"},
		Arguments: json.RawMessage(outcomeNotNullArgs),
	})
	require.NoError(t, err)
	outcome := result.(MigrationOutcome)
	require.Equal(t, "recommend_only", outcome.Verdict)
	require.Equal(t, ReasonNoRehearsalRuntime, outcome.Reason)
}

type fixedRehearser struct{ result rehearsalpkg.Result }

func (r fixedRehearser) Rehearse(context.Context, plan.Plan) (rehearsalpkg.Result, error) {
	return r.result, nil
}

type noopApplier struct{}

func (noopApplier) Apply(context.Context, plan.Step) error { return nil }

type noopRecorder struct{}

func (noopRecorder) Record(context.Context, migrationruntime.Record) error { return nil }
