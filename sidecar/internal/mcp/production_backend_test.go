package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestEnabledStdioRuntimeServesWithProductionBackend(t *testing.T) {
	fixture := newProductionFixture()
	backend, err := NewProductionBackend(fixture.dependencies())
	require.NoError(t, err)
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
			`"params":{"name":"get_policy","arguments":{"database_id":42}}}` + "\n",
	)
	output := &bytes.Buffer{}
	runtime, err := NewRuntime(
		config.MCPConfig{Enabled: true, Transport: "stdio"},
		NewServer(backend), input, output,
	)
	require.NoError(t, err)

	require.NoError(t, runtime.Serve(context.Background()))

	var response protocolResponse
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response))
	require.Empty(t, response.Error.Code)
	payload := structuredContent(t, response)
	require.Equal(t, float64(7), payload["version"])
	require.Equal(t, "unattended", payload["profile"])
	require.Equal(t, 1, fixture.policyReads)
}

func TestEveryMutatingIntentUsesSameStandingPolicyGate(t *testing.T) {
	testCases := []struct {
		name string
		call func(context.Context, *ProductionBackend) error
	}{
		{
			name: "generic change request",
			call: func(ctx context.Context, backend *ProductionBackend) error {
				_, err := backend.RequestChange(ctx, ChangeRequest{
					Intent: json.RawMessage(`{"kind":"optimize_query","query_id":991}`),
				})
				return err
			},
		},
		{
			name: "optimize query",
			call: namedIntentCall("optimize_query", `{"query_id":991,"goal":"latency"}`),
		},
		{
			name: "apply migration",
			call: namedIntentCall("apply_migration", `{"intent":{"add_unique":"email"}}`),
		},
		{
			name: "ensure foreign key indexes",
			call: namedIntentCall("ensure_fk_indexes", `{"schema":"public"}`),
		},
		{
			name: "declare table contract",
			call: namedIntentCall("declare_table_contract", `{"table":"public.events"}`),
		},
		{
			name: "register consumer",
			call: namedIntentCall(
				"register_consumer", `{"slot_name":"orders_cdc","owner":"agent-a"}`,
			),
		},
	}

	fixture := newProductionFixture()
	fixture.gate.decision = policy.Decision{
		Verdict: policy.VerdictPark, Reason: policy.ReasonOutsideMaintenanceWindow,
		EvidenceID: "ev-policy",
	}
	backend, err := NewProductionBackend(fixture.dependencies())
	require.NoError(t, err)

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			beforeGate := fixture.gate.calls
			beforeApply := fixture.executorCalls()

			require.NoError(t, testCase.call(context.Background(), backend))

			require.Equal(t, beforeGate+1, fixture.gate.calls)
			require.Equal(t, beforeApply, fixture.executorCalls())
		})
	}
}

func TestCallerClaimsCannotWidenStandingPolicyAuthority(t *testing.T) {
	fixture := newProductionFixture()
	fixture.gate.decision = policy.Decision{
		Verdict: policy.VerdictBlocked, Reason: policy.ReasonChangeClassNotAllowed,
		EvidenceID: "ev-denied",
	}
	backend, err := NewProductionBackend(fixture.dependencies())
	require.NoError(t, err)

	result, err := backend.RequestChange(context.Background(), ChangeRequest{
		Intent: json.RawMessage(`{"kind":"apply_migration","table":"public.users"}`),
		CallerClaims: map[string]any{
			"role": "admin", "autonomous_approved": true, "skip_policy": true,
		},
	})

	require.NoError(t, err)
	require.Equal(t, "blocked", result.Decision)
	require.Equal(t, "ev-denied", result.EvidenceID)
	require.Equal(t, string(policy.ReasonChangeClassNotAllowed), result.Reason)
	require.Equal(t, 1, fixture.gate.calls)
	require.Zero(t, fixture.executorCalls())
	require.NotContains(t, fixture.plannerArguments(), "skip_policy")
	require.NotContains(t, fixture.plannerArguments(), "autonomous_approved")
}

func TestPolicyProposalIsDryRunOnlyAndCannotRatifyOrApply(t *testing.T) {
	fixture := newProductionFixture()
	backend, err := NewProductionBackend(fixture.dependencies())
	require.NoError(t, err)

	result, err := backend.ProposePolicyChange(
		context.Background(),
		PolicyProposalRequest{
			DatabaseID: int64ProductionPointer(42),
			Delta: json.RawMessage(
				`{"budgets":{"storage_bytes":0}}`,
			),
			CallerClaims: map[string]any{"role": "admin", "ratify": true},
		},
	)

	require.NoError(t, err)
	require.Equal(t, int64(19), result.ProposalID)
	require.Equal(t, []string{"action-7"}, result.DryRunImpact.NewlyAllowed)
	require.Equal(t, []string{"action-9"}, result.DryRunImpact.NewlyBlocked)
	require.Equal(t, 1, fixture.policyProposals)
	require.Zero(t, fixture.policyRatifications)
	require.Zero(t, fixture.gate.calls)
	require.Zero(t, fixture.executorCalls())
}

func TestProductionReadToolsReturnMachineJSON(t *testing.T) {
	fixture := newProductionFixture()
	backend, err := NewProductionBackend(fixture.dependencies())
	require.NoError(t, err)
	server := NewServer(backend)
	testCases := []struct {
		name   string
		tool   string
		args   string
		assert func(*testing.T, map[string]any)
	}{
		{
			name: "policy", tool: "get_policy", args: `{"database_id":42}`,
			assert: func(t *testing.T, payload map[string]any) {
				require.Equal(t, json.Number("7"), payload["version"])
				require.Equal(t, "unattended", payload["profile"])
			},
		},
		{
			name: "value", tool: "get_value", args: `{}`,
			assert: func(t *testing.T, payload map[string]any) {
				hours := objectMap(t, payload["dba_hours_saved"])
				require.Equal(t, json.Number("12.5"), hours["all_time"])
				require.Equal(t, json.Number("2.5"), payload["potential_hours_pending"])
			},
		},
		{
			name: "ledger", tool: "get_ledger", args: `{"filter":{"limit":10}}`,
			assert: func(t *testing.T, payload map[string]any) {
				entries, ok := payload["entries"].([]any)
				require.True(t, ok)
				require.Len(t, entries, 1)
				require.Equal(t, "ev-123", objectMap(t, entries[0])["evidence_id"])
			},
		},
	}

	for index, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := `{"jsonrpc":"2.0","id":` + jsonNumber(index+1) +
				`,"method":"tools/call","params":{"name":"` + testCase.tool +
				`","arguments":` + testCase.args + `}}`
			response := invoke(t, server, context.Background(), request)
			require.Empty(t, response.Error.Code)
			testCase.assert(t, structuredContent(t, response))
		})
	}
}

func namedIntentCall(
	tool string, arguments string,
) func(context.Context, *ProductionBackend) error {
	return func(ctx context.Context, backend *ProductionBackend) error {
		_, err := backend.RequestIntent(ctx, tool, json.RawMessage(arguments))
		return err
	}
}

type productionFixture struct {
	gate                *recordingProductionGate
	planner             IntentPlanner
	executor            IntentExecutor
	guarantees          GuaranteeAccess
	policyReads         int
	policyProposals     int
	policyRatifications int
	ledgerReads         int
	valueReads          int
}

func newProductionFixture() *productionFixture {
	return &productionFixture{
		gate: &recordingProductionGate{decision: policy.Decision{
			Verdict: policy.VerdictExecute, Reason: policy.ReasonAuthorized,
			EvidenceID: "ev-authorized",
		}},
		planner:  &recordingProductionPlanner{},
		executor: &recordingProductionExecutor{},
		guarantees: &recordingGuaranteeAccess{status: GuaranteeStatus{
			XID: map[string]any{}, WAL: map[string]any{}, Schema: map[string]any{},
		}},
	}
}

func (fixture *productionFixture) dependencies() ProductionDependencies {
	return ProductionDependencies{
		Gate:       fixture.gate,
		Planner:    fixture.planner,
		Executor:   fixture.executor,
		Policy:     fixture,
		Ledger:     fixture,
		Value:      fixture,
		Guarantees: fixture.guarantees,
	}
}

func (fixture *productionFixture) GetPolicy(
	_ context.Context, request PolicyRequest,
) (PolicyResult, error) {
	fixture.policyReads++
	return PolicyResult{
		DatabaseID: request.DatabaseID, Version: 7, Profile: "unattended",
	}, nil
}

func (fixture *productionFixture) ProposePolicyChangeDryRun(
	_ context.Context, _ PolicyProposalRequest,
) (PolicyProposalResult, error) {
	fixture.policyProposals++
	return PolicyProposalResult{
		ProposalID: 19,
		DryRunImpact: DryRunImpact{
			NewlyAllowed: []string{"action-7"},
			NewlyBlocked: []string{"action-9"},
		},
	}, nil
}

func (fixture *productionFixture) RatifyPolicyForTest() {
	fixture.policyRatifications++
}

func (fixture *productionFixture) GetLedger(
	_ context.Context, _ LedgerRequest,
) (LedgerResult, error) {
	fixture.ledgerReads++
	return LedgerResult{Entries: []LedgerEntry{
		{EvidenceID: "ev-123", Decision: "parked", Feature: "index"},
	}}, nil
}

func (fixture *productionFixture) GetValue(context.Context) (map[string]any, error) {
	fixture.valueReads++
	return map[string]any{
		"dba_hours_saved":         map[string]any{"all_time": 12.5},
		"potential_hours_pending": 2.5,
	}, nil
}

type recordingProductionGate struct {
	decision policy.Decision
	calls    int
	requests []policy.ActionRequest
}

func (gate *recordingProductionGate) Authorize(
	_ context.Context, request policy.ActionRequest,
) policy.Decision {
	gate.calls++
	gate.requests = append(gate.requests, request)
	return gate.decision
}

type recordingProductionPlanner struct {
	lastTool      string
	lastArguments string
}

func (planner *recordingProductionPlanner) Plan(
	_ context.Context, tool string, arguments json.RawMessage,
) (policy.ActionRequest, error) {
	planner.lastTool = tool
	planner.lastArguments = string(arguments)
	return policy.ActionRequest{
		Contract: &policy.ActionContract{
			ActionType: tool, RiskTier: policy.RiskSafe,
		},
		Feature: tool,
	}, nil
}

type recordingProductionExecutor struct {
	calls int
}

func (fixture *productionFixture) executorCalls() int {
	executor, ok := fixture.executor.(*recordingProductionExecutor)
	if !ok {
		return 0
	}
	return executor.calls
}

func (fixture *productionFixture) plannerArguments() string {
	planner, ok := fixture.planner.(*recordingProductionPlanner)
	if !ok {
		return ""
	}
	return planner.lastArguments
}

func (executor *recordingProductionExecutor) Execute(
	_ context.Context, _ policy.ActionRequest, _ policy.Decision,
) (any, error) {
	executor.calls++
	return map[string]any{"applied": true}, nil
}

func int64ProductionPointer(value int64) *int64 {
	return &value
}

func jsonNumber(value int) string {
	return strconv.Itoa(value)
}
