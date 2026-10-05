package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// The Postgres-specialist contract over MCP: the same investigation
// contract other agents call over HTTP. Opening and reading need read;
// requesting a remediation needs propose and is still only a proposal.

type specialistRecordingBackend struct {
	recordingBackend
	calls    int
	tool     string
	caller   SpecialistCaller
	database string
	args     json.RawMessage
	result   any
	err      error
}

func (b *specialistRecordingBackend) SpecialistCall(_ context.Context, tool string,
	caller SpecialistCaller, database string, args json.RawMessage) (any, error) {
	b.calls++
	b.tool, b.caller, b.database, b.args = tool, caller, database, args
	if b.result == nil {
		b.result = map[string]any{"contract_version": "pg_sage.specialist.v1"}
	}
	return b.result, b.err
}

type codedErr string

func (e codedErr) Error() string          { return "specialist: " + string(e) }
func (e codedErr) SpecialistCode() string { return string(e) }

var specialistToolArgs = map[string]string{
	"specialist_open_investigation": `{"database":"orders","symptom":{"summary":"slow"},` +
		`"family":"lock_blocking"}`,
	"specialist_investigation_status": `{"database":"orders",` +
		`"investigation_id":"44444444-4444-4444-8444-444444444444"}`,
	"specialist_investigation_result": `{"database":"orders",` +
		`"investigation_id":"44444444-4444-4444-8444-444444444444"}`,
	"specialist_request_remediation": `{"database":"orders",` +
		`"investigation_id":"44444444-4444-4444-8444-444444444444",` +
		`"remediation_id":"custodian.0123456789abcdef","reason":"PD Q1"}`,
}

func agentCtx(scopes ...Scope) context.Context {
	return WithPrincipal(context.Background(), Principal{Actor: "token:t-1",
		Name: "PagerDuty", Kind: KindAgent, Scopes: scopes, Databases: []string{"orders"},
		TokenID: "t-1"})
}

func TestSpecialistToolsAreListedWithScopes(t *testing.T) {
	tools := map[string]Tool{}
	for _, tool := range NewServer(&specialistRecordingBackend{}).Tools() {
		tools[tool.Name] = tool
	}
	for name := range specialistToolArgs {
		tool, ok := tools[name]
		require.True(t, ok, name)
		scope, known := RequiredScope(name, json.RawMessage(`{}`))
		require.True(t, known, name)
		if name == "specialist_request_remediation" {
			require.Equal(t, ScopePropose, scope)
			require.False(t, tool.Annotations.ReadOnlyHint)
		} else {
			require.Equal(t, ScopeRead, scope, name)
			require.True(t, tool.Annotations.ReadOnlyHint, name)
		}
		var schema map[string]any
		require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
		require.Equal(t, false, schema["additionalProperties"], name)
	}
}

func TestSpecialistTools_CallerAndArgumentsReachTheBackend(t *testing.T) {
	backend := &specialistRecordingBackend{}
	response := invoke(t, NewServer(backend), agentCtx(ScopeRead),
		toolCall("specialist_open_investigation",
			specialistToolArgs["specialist_open_investigation"]))
	require.Empty(t, response.Error.Code)
	require.Equal(t, 1, backend.calls)
	require.Equal(t, "specialist_open_investigation", backend.tool)
	require.Equal(t, "orders", backend.database)
	require.Equal(t, "PagerDuty", backend.caller.Name)
	require.Equal(t, "t-1", backend.caller.TokenID)
	require.Equal(t, "agent", backend.caller.Kind)
	require.Equal(t, []string{"read"}, backend.caller.Scopes)
	require.Equal(t, []string{"orders"}, backend.caller.Databases)
	var args map[string]any
	require.NoError(t, json.Unmarshal(backend.args, &args))
	_, hasDatabase := args["database"]
	require.False(t, hasDatabase, "the database travels separately")
	require.Equal(t, "lock_blocking", args["family"])
}

func TestSpecialistTools_ReadScopeCannotRequestARemediation(t *testing.T) {
	backend := &specialistRecordingBackend{}
	response := invoke(t, NewServer(backend), agentCtx(ScopeRead),
		toolCall("specialist_request_remediation",
			specialistToolArgs["specialist_request_remediation"]))
	require.Equal(t, codeScopeRequired, response.Error.Code)
	require.Equal(t, 0, backend.calls)
	backend = &specialistRecordingBackend{}
	response = invoke(t, NewServer(backend), agentCtx(ScopeRead, ScopePropose),
		toolCall("specialist_request_remediation",
			specialistToolArgs["specialist_request_remediation"]))
	require.Empty(t, response.Error.Code)
	require.Equal(t, []string{"read", "propose"}, backend.caller.Scopes)
}

func TestSpecialistTools_OtherDatabaseIsRefused(t *testing.T) {
	backend := &specialistRecordingBackend{}
	response := invoke(t, NewServer(backend), agentCtx(ScopeRead),
		toolCall("specialist_investigation_status", `{"database":"billing",`+
			`"investigation_id":"44444444-4444-4444-8444-444444444444"}`))
	require.Equal(t, codeNotPermitted, response.Error.Code)
	require.Equal(t, 0, backend.calls)
}

func TestSpecialistTools_StrictArguments(t *testing.T) {
	backend := &specialistRecordingBackend{}
	for name, args := range map[string]string{
		"specialist_investigation_status": `{"database":"orders"}`,
		"specialist_request_remediation": `{"database":"orders","investigation_id":` +
			`"44444444-4444-4444-8444-444444444444","remediation_id":"x","force":true}`,
		"specialist_investigation_result": `{"database":"orders","investigation_id":7}`,
	} {
		response := invoke(t, NewServer(backend), agentCtx(ScopeRead, ScopePropose),
			toolCall(name, args))
		require.Equal(t, codeInvalidParams, response.Error.Code, name)
	}
	require.Equal(t, 0, backend.calls)
}

func TestSpecialistTools_ErrorsAreDistinguishable(t *testing.T) {
	cases := map[string]int{"scope_required": codeScopeRequired,
		"database_not_permitted": codeNotPermitted, "not_found": codeNotFound,
		"invalid_request": codeInvalidParams, "rate_limited": codeRateLimited,
		"too_many_investigations": codeTooManyInvestigations,
		"not_requestable": codeConflict, "unavailable": codeUnavailable,
		"payload_too_large": codeInvalidParams, "internal": codeInternal}
	for code, want := range cases {
		backend := &specialistRecordingBackend{err: codedErr(code)}
		response := invoke(t, NewServer(backend), agentCtx(ScopeRead),
			toolCall("specialist_investigation_result",
				specialistToolArgs["specialist_investigation_result"]))
		require.Equal(t, want, response.Error.Code, code)
	}
}

func TestSpecialistTools_UnavailableWithoutABackend(t *testing.T) {
	response := invoke(t, NewServer(&recordingBackend{}), agentCtx(ScopeRead),
		toolCall("specialist_investigation_status",
			specialistToolArgs["specialist_investigation_status"]))
	require.Equal(t, codeUnavailable, response.Error.Code)
}

func TestSpecialistCallerFromSessionPrincipal(t *testing.T) {
	// A principal without explicit scopes (a session role) gets the scopes
	// its role grants; approve never travels.
	backend := &specialistRecordingBackend{}
	ctx := WithPrincipal(context.Background(), Principal{Actor: "user:7", Role: "operator"})
	response := invoke(t, NewServer(backend), ctx, toolCall("specialist_investigation_status",
		specialistToolArgs["specialist_investigation_status"]))
	require.Empty(t, response.Error.Code)
	require.Equal(t, []string{"read", "propose"}, backend.caller.Scopes)
	require.Equal(t, "user:7", backend.caller.TokenID)
}
