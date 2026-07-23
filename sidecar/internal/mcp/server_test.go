package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestToolRegistryExposesF6IntentsWithoutRawSQL(t *testing.T) {
	server := NewServer(&recordingBackend{})
	tools := server.Tools()

	byName := make(map[string]Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{
		"get_policy",
		"propose_policy_change",
		"request_change",
		"optimize_query",
		"apply_migration",
		"ensure_fk_indexes",
		"declare_table_contract",
		"register_consumer",
		"set_maintenance_policy",
		"get_guarantee_status",
		"get_value",
		"get_ledger",
	} {
		require.Contains(t, byName, name)
	}
	for _, name := range []string{
		"execute_sql",
		"query_sql",
		"raw_sql",
		"run_ddl",
	} {
		require.NotContains(t, byName, name)
	}

	changeSchema := decodeSchema(t, byName["request_change"].InputSchema)
	require.Contains(t, stringSlice(changeSchema["required"]), "intent")
	properties := objectMap(t, changeSchema["properties"])
	require.Contains(t, properties, "intent")
	require.NotContains(t, properties, "sql")
	require.NotContains(t, properties, "ddl")

	proposalSchema := decodeSchema(t, byName["propose_policy_change"].InputSchema)
	require.Contains(t, stringSlice(proposalSchema["required"]), "delta")
}

func TestInitializeAndToolsListAreValidJSONRPC(t *testing.T) {
	server := NewServer(&recordingBackend{})

	initialize := invoke(t, server, context.Background(), `{
		"jsonrpc":"2.0","id":1,"method":"initialize",
		"params":{"protocolVersion":"2025-03-26","capabilities":{},
		"clientInfo":{"name":"test-agent","version":"1"}}
	}`)
	require.Equal(t, "2.0", initialize.JSONRPC)
	require.Equal(t, json.Number("1"), initialize.ID)
	require.Empty(t, initialize.Error.Code)
	result := objectMap(t, initialize.Result)
	require.Equal(t, "2025-03-26", result["protocolVersion"])
	require.Contains(t, objectMap(t, result["capabilities"]), "tools")
	serverInfo := objectMap(t, result["serverInfo"])
	require.Equal(t, "pg_sage", serverInfo["name"])

	listed := invoke(t, server, context.Background(), `{
		"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}
	}`)
	require.Empty(t, listed.Error.Code)
	listedResult := objectMap(t, listed.Result)
	tools, ok := listedResult["tools"].([]any)
	require.True(t, ok)
	require.GreaterOrEqual(t, len(tools), 4)
}

func TestPolicyToolsRouteTypedRequestsToPolicyBackend(t *testing.T) {
	databaseID := int64(42)
	backend := &recordingBackend{
		policyResult: PolicyResult{
			DatabaseID: &databaseID,
			Version:    7,
			Profile:    "unattended",
		},
		proposalResult: PolicyProposalResult{
			ProposalID: 19,
			DryRunImpact: DryRunImpact{
				NewlyAllowed: []string{"action-7"},
				NewlyBlocked: []string{"action-9"},
			},
		},
	}
	server := NewServer(backend)

	policyResponse := invoke(t, server, context.Background(), `{
		"jsonrpc":"2.0","id":1,"method":"tools/call",
		"params":{"name":"get_policy","arguments":{"database_id":42}}
	}`)
	require.Empty(t, policyResponse.Error.Code)
	require.Equal(t, int64(42), *backend.policyRequest.DatabaseID)
	policyPayload := structuredContent(t, policyResponse)
	require.Equal(t, json.Number("7"), policyPayload["version"])
	require.Equal(t, "unattended", policyPayload["profile"])

	proposalResponse := invoke(t, server, context.Background(), `{
		"jsonrpc":"2.0","id":2,"method":"tools/call",
		"params":{"name":"propose_policy_change","arguments":{
			"database_id":42,
			"delta":{"budgets":{"storage_bytes":0}},
			"caller_claims":{"role":"admin","autonomous_approved":true}
		}}
	}`)
	require.Empty(t, proposalResponse.Error.Code)
	require.Equal(t, int64(42), *backend.proposalRequest.DatabaseID)
	require.JSONEq(t, `{"budgets":{"storage_bytes":0}}`,
		string(backend.proposalRequest.Delta))
	require.Equal(t, "admin", backend.proposalRequest.CallerClaims["role"])
	proposalPayload := structuredContent(t, proposalResponse)
	require.Equal(t, json.Number("19"), proposalPayload["proposal_id"])
	impact := objectMap(t, proposalPayload["dry_run_impact"])
	require.Equal(t, []any{"action-7"}, impact["newly_allowed"])
	require.Equal(t, []any{"action-9"}, impact["newly_blocked"])
}

func TestRequestChangeRoutesIntentThroughPolicyWithoutTrustingClaims(t *testing.T) {
	backend := &recordingBackend{
		changeResult: ChangeResult{
			Decision:   "parked",
			EvidenceID: "ev-123",
			Reason:     "outside_policy",
		},
	}
	server := NewServer(backend)

	response := invoke(t, server, context.Background(), `{
		"jsonrpc":"2.0","id":3,"method":"tools/call",
		"params":{"name":"request_change","arguments":{
			"database_id":42,
			"intent":{"kind":"optimize_query","query_id":991,"goal":"latency"},
			"caller_claims":{"role":"admin","skip_policy":true}
		}}
	}`)

	require.Empty(t, response.Error.Code)
	require.Equal(t, int64(42), *backend.changeRequest.DatabaseID)
	require.JSONEq(t, `{"kind":"optimize_query","query_id":991,"goal":"latency"}`,
		string(backend.changeRequest.Intent))
	require.Equal(t, true, backend.changeRequest.CallerClaims["skip_policy"])
	payload := structuredContent(t, response)
	require.Equal(t, "parked", payload["decision"])
	require.Equal(t, "ev-123", payload["evidence_id"])
	require.Equal(t, "outside_policy", payload["reason"])
}

func TestGetLedgerRoutesMachineReadableFilter(t *testing.T) {
	backend := &recordingBackend{
		ledgerResult: LedgerResult{Entries: []LedgerEntry{
			{
				EvidenceID: "ev-123",
				Decision:   "parked",
				Feature:    "index",
			},
		}},
	}
	server := NewServer(backend)

	response := invoke(t, server, context.Background(), `{
		"jsonrpc":"2.0","id":4,"method":"tools/call",
		"params":{"name":"get_ledger","arguments":{
			"filter":{"database_id":42,"decision":"parked","limit":25}
		}}
	}`)

	require.Empty(t, response.Error.Code)
	require.JSONEq(t, `{"database_id":42,"decision":"parked","limit":25}`,
		string(backend.ledgerRequest.Filter))
	payload := structuredContent(t, response)
	entries, ok := payload["entries"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 1)
	entry := objectMap(t, entries[0])
	require.Equal(t, "ev-123", entry["evidence_id"])
	require.Equal(t, "parked", entry["decision"])
}

func TestMalformedAndInvalidRequestsReturnProtocolErrors(t *testing.T) {
	testCases := []struct {
		name string
		body string
		code int
	}{
		{name: "malformed JSON", body: `{`, code: -32700},
		{name: "wrong JSON-RPC version", body: `{
			"jsonrpc":"1.0","id":1,"method":"tools/list","params":{}
		}`, code: -32600},
		{name: "missing method", body: `{
			"jsonrpc":"2.0","id":1,"params":{}
		}`, code: -32600},
		{name: "unknown method", body: `{
			"jsonrpc":"2.0","id":1,"method":"database/execute","params":{}
		}`, code: -32601},
		{name: "unknown tool", body: `{
			"jsonrpc":"2.0","id":1,"method":"tools/call",
			"params":{"name":"execute_sql","arguments":{"sql":"DROP TABLE users"}}
		}`, code: -32601},
		{name: "missing intent", body: `{
			"jsonrpc":"2.0","id":1,"method":"tools/call",
			"params":{"name":"request_change","arguments":{"database_id":42}}
		}`, code: -32602},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			backend := &recordingBackend{}
			response := invoke(t, NewServer(backend), context.Background(), testCase.body)
			require.Equal(t, testCase.code, response.Error.Code)
			require.Equal(t, 0, backend.totalCalls())
		})
	}
}

func TestCancellationPropagatesAndReturnsRequestCancelled(t *testing.T) {
	backend := &recordingBackend{}
	backend.changeFunc = func(ctx context.Context, _ ChangeRequest) (ChangeResult, error) {
		backend.changeContextErr = ctx.Err()
		return ChangeResult{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	response := invoke(t, NewServer(backend), ctx, `{
		"jsonrpc":"2.0","id":7,"method":"tools/call",
		"params":{"name":"request_change","arguments":{
			"intent":{"kind":"optimize_query","query_id":991}
		}}
	}`)

	require.Equal(t, -32800, response.Error.Code)
	require.ErrorIs(t, backend.changeContextErr, context.Canceled)
	require.NotContains(t, response.Error.Message, "secret")
}

func TestBackendFailureIsSanitized(t *testing.T) {
	backend := &recordingBackend{
		ledgerErr: errors.New("postgres password=secret connection failed"),
	}

	response := invoke(t, NewServer(backend), context.Background(), `{
		"jsonrpc":"2.0","id":8,"method":"tools/call",
		"params":{"name":"get_ledger","arguments":{"filter":{}}}
	}`)

	require.Equal(t, -32603, response.Error.Code)
	require.NotContains(t, response.Error.Message, "password")
	require.NotContains(t, response.Error.Message, "secret")
}

type recordingBackend struct {
	policyRequest    PolicyRequest
	policyResult     PolicyResult
	policyErr        error
	policyCalls      int
	proposalRequest  PolicyProposalRequest
	proposalResult   PolicyProposalResult
	proposalErr      error
	proposalCalls    int
	changeRequest    ChangeRequest
	changeResult     ChangeResult
	changeErr        error
	changeFunc       func(context.Context, ChangeRequest) (ChangeResult, error)
	changeContextErr error
	changeCalls      int
	ledgerRequest    LedgerRequest
	ledgerResult     LedgerResult
	ledgerErr        error
	ledgerCalls      int
}

func (backend *recordingBackend) GetPolicy(
	_ context.Context, request PolicyRequest,
) (PolicyResult, error) {
	backend.policyCalls++
	backend.policyRequest = request
	return backend.policyResult, backend.policyErr
}

func (backend *recordingBackend) ProposePolicyChange(
	_ context.Context, request PolicyProposalRequest,
) (PolicyProposalResult, error) {
	backend.proposalCalls++
	backend.proposalRequest = request
	return backend.proposalResult, backend.proposalErr
}

func (backend *recordingBackend) RequestChange(
	ctx context.Context, request ChangeRequest,
) (ChangeResult, error) {
	backend.changeCalls++
	backend.changeRequest = request
	if backend.changeFunc != nil {
		return backend.changeFunc(ctx, request)
	}
	return backend.changeResult, backend.changeErr
}

func (backend *recordingBackend) GetLedger(
	_ context.Context, request LedgerRequest,
) (LedgerResult, error) {
	backend.ledgerCalls++
	backend.ledgerRequest = request
	return backend.ledgerResult, backend.ledgerErr
}

func (backend *recordingBackend) totalCalls() int {
	return backend.policyCalls + backend.proposalCalls + backend.changeCalls +
		backend.ledgerCalls
}

type protocolResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.Number     `json:"id"`
	Result  any             `json:"result"`
	Error   protocolFailure `json:"error"`
}

type protocolFailure struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func invoke(
	t *testing.T, server *Server, ctx context.Context, request string,
) protocolResponse {
	t.Helper()
	encoded := server.Handle(ctx, json.RawMessage(request))
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var response protocolResponse
	require.NoError(t, decoder.Decode(&response))
	return response
}

func structuredContent(t *testing.T, response protocolResponse) map[string]any {
	t.Helper()
	result := objectMap(t, response.Result)
	return objectMap(t, result["structuredContent"])
}

func decodeSchema(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var result map[string]any
	require.NoError(t, decoder.Decode(&result))
	return result
}

func objectMap(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	require.True(t, ok, "value is not a JSON object: %#v", value)
	return result
}

func stringSlice(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if ok {
			result = append(result, text)
		}
	}
	return result
}
