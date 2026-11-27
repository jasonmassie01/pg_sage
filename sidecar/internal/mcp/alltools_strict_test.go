package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/specialist"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Strict arguments (mcp-08): every tool's schema says
// additionalProperties:false, so every tool refuses a name its schema
// does not list. The calls go through callTool, so the database and
// database_id the server injects are present, as in production.

// strictBackend adds Ask Sage to the all-tools census backend.
type strictBackend struct{ *allToolsBackend }

func (b strictBackend) AskSage(ctx context.Context, database string, _ ask.Caller,
	r ask.Request) (ask.Answer, error) {
	b.hit(ctx, "ask_sage")
	return ask.Answer{ID: 1, Database: database, Question: r.Question}, nil
}

// GetTranscript adds sre_get_transcript to the census backend.
func (b strictBackend) GetTranscript(ctx context.Context, _ InvestigationRequest,
	_ bool) (any, error) {
	b.hit(ctx, "sre_get_transcript")
	return map[string]any{}, nil
}

// SpecialistCall decodes the open request with the contract's strict
// decoder, as the production adapter does (SpecialistService): the
// server leaves that request to the contract.
func (b strictBackend) SpecialistCall(ctx context.Context, tool string,
	caller SpecialistCaller, database string, args json.RawMessage) (any, error) {
	if tool == "specialist_open_investigation" {
		if _, err := specialist.DecodeOpenRequest(bytes.NewReader(args)); err != nil {
			return nil, err
		}
	}
	return b.allToolsBackend.SpecialistCall(ctx, tool, caller, database, args)
}

var strictExtraArgs = map[string]string{
	"ask_sage":           `{"question":"why is checkout slow?"}`,
	"sre_get_transcript": `{"investigation_id":"5f0c2a52-0d55-4a43-9a3c-0d6c1f6c9a11"}`,
}

func strictArgs(b *allToolsBackend, tool string) string {
	if args, ok := strictExtraArgs[tool]; ok {
		return args
	}
	return b.validArgs(tool)
}

// strictContext is a person with every scope, except for the one tool
// only an agent principal may call.
func strictContext(tool string) context.Context {
	if tool == toolRequestCapability {
		return agentContext(ScopeRead, ScopePropose)
	}
	return WithPrincipal(context.Background(), Principal{Actor: "user:1", Role: "admin"})
}

func withUnknownKey(t *testing.T, args string) string {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(args), &object))
	object["zz_unknown"] = json.RawMessage(`1`)
	raw, err := json.Marshal(object)
	require.NoError(t, err)
	return string(raw)
}

func strictServer(b *allToolsBackend) *Server {
	return NewServer(strictBackend{b}).WithDirectory(fleetOf("orders"))
}

func TestStrictEveryToolRejectsAnUnknownArgument(t *testing.T) {
	tools := NewServer(strictBackend{newAllToolsBackend()}).Tools()
	require.GreaterOrEqual(t, len(tools), 53)
	for _, tool := range tools {
		t.Run(tool.Name, func(t *testing.T) {
			backend := newAllToolsBackend()
			args := withUnknownKey(t, strictArgs(backend, tool.Name))
			response := invoke(t, strictServer(backend), strictContext(tool.Name),
				toolCall(tool.Name, args))
			require.Equal(t, codeInvalidParams, response.Error.Code, response.Error.Message)
			require.Zero(t, backend.calls(), "reached its backend")
		})
	}
}

func TestStrictEveryToolAcceptsItsSchemaArguments(t *testing.T) {
	for _, tool := range NewServer(strictBackend{newAllToolsBackend()}).Tools() {
		t.Run(tool.Name, func(t *testing.T) {
			backend := newAllToolsBackend()
			response := invoke(t, strictServer(backend), strictContext(tool.Name),
				toolCall(tool.Name, strictArgs(backend, tool.Name)))
			require.NotEqual(t, codeInvalidParams, response.Error.Code,
				response.Error.Message)
		})
	}
}

func TestStrictEveryToolSchemaForbidsAdditionalProperties(t *testing.T) {
	for _, tool := range NewServer(strictBackend{newAllToolsBackend()}).Tools() {
		schema := decodeSchema(t, tool.InputSchema)
		require.Equal(t, false, schema["additionalProperties"], tool.Name)
	}
}

// intentToolNames are the intent tools, whose arguments the server
// decodes itself; their happy path must reach the backend.
var intentToolNames = []string{"get_policy", "propose_policy_change", "request_change",
	"get_ledger", "optimize_query", "apply_migration", "ensure_fk_indexes",
	"declare_table_contract", "register_consumer", "set_maintenance_policy",
	"get_guarantee_status", "get_value"}

func TestStrictIntentToolsReachTheBackendWithInjectedDatabase(t *testing.T) {
	for _, name := range intentToolNames {
		backend := newAllToolsBackend()
		response := invoke(t, strictServer(backend), strictContext(name),
			toolCall(name, strictArgs(backend, name)))
		require.Empty(t, response.Error.Code, "%s: %s", name, response.Error.Message)
		require.Equal(t, 1, backend.calls(), name)
		require.Equal(t, "orders", backend.ctxDB, name)
	}
}

func TestStrictIntentToolsRejectMistypedArguments(t *testing.T) {
	cases := map[string]string{
		"get_ledger":             `{"filter":"recent"}`,
		"propose_policy_change":  `{"delta":[1]}`,
		"request_change":         `{"intent":"optimize everything"}`,
		"optimize_query":         `{"goal":"latency","query_id":"one"}`,
		"optimize_query ":        `{"goal":"latency","constraints":[]}`,
		"ensure_fk_indexes":      `{"schema":7}`,
		"declare_table_contract": `{"table":"public.t","exemptions":"all"}`,
		"declare_table_contract ": `{"table":"public.t","retention":` +
			`{"interval":30,"column":"created_at"}}`,
		"register_consumer":       `{"slot_name":"s1","owner":false}`,
		"set_maintenance_policy":  `{"scope":{},"patch":"vacuum more"}`,
		"set_maintenance_policy ": `{"scope":"all","patch":{"x":1}}`,
		"get_value":               `{"databse_id":1}`,
		"get_guarantee_status":    `{"verbose":true}`,
		"get_policy":              `{"profile":"unattended"}`,
	}
	for label, args := range cases {
		name := trimLabel(label)
		backend := newAllToolsBackend()
		response := invoke(t, strictServer(backend), strictContext(name),
			toolCall(name, args))
		require.Equal(t, codeInvalidParams, response.Error.Code, "%s %s: %s", name, args,
			response.Error.Message)
		require.Contains(t, response.Error.Message, "invalid arguments", label)
		require.Zero(t, backend.calls(), label)
	}
}

// trimLabel lets one tool appear more than once as a map key.
func trimLabel(label string) string {
	for len(label) > 0 && label[len(label)-1] == ' ' {
		label = label[:len(label)-1]
	}
	return label
}

// Boundary: the client may send database_id itself; the server then
// injects the same value again, which is not an unknown name.
func TestStrictClientAndInjectedDatabaseIDAreAccepted(t *testing.T) {
	backend := newAllToolsBackend()
	response := invoke(t, strictServer(backend), strictContext("request_change"),
		toolCall("request_change", `{"database":"orders","database_id":1,`+
			`"intent":{"kind":"optimize_query","query_id":1}}`))
	require.Empty(t, response.Error.Code, response.Error.Message)
	require.True(t, backend.change.DatabaseID != nil, "database_id dropped")
	require.Equal(t, int64(1), *backend.change.DatabaseID)

	for _, name := range []string{"get_ledger", "optimize_query", "get_value"} {
		backend := newAllToolsBackend()
		response := invoke(t, strictServer(backend), strictContext(name),
			toolCall(name, withDatabaseID(t, strictArgs(backend, name))))
		require.Empty(t, response.Error.Code, "%s: %s", name, response.Error.Message)
		require.Equal(t, 1, backend.calls(), name)
	}
}

func withDatabaseID(t *testing.T, args string) string {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(args), &object))
	object["database_id"] = json.RawMessage(`1`)
	raw, err := json.Marshal(object)
	require.NoError(t, err)
	return string(raw)
}

// Boundary: the injected database_id reaches a raw-argument intent so
// the backend can still read it.
func TestStrictRawIntentKeepsInjectedDatabaseID(t *testing.T) {
	backend := newAllToolsBackend()
	response := invoke(t, strictServer(backend), strictContext("optimize_query"),
		toolCall("optimize_query", `{"goal":"latency","query_id":1}`))
	require.Empty(t, response.Error.Code, response.Error.Message)
	require.Equal(t, int64(1), *databaseIDFromJSON(backend.intent))
}

// Nil/empty: absent and null arguments are an empty object, which the
// read intents accept.
func TestStrictEmptyArgumentsAreAnEmptyObject(t *testing.T) {
	for _, raw := range []string{`{}`, `null`} {
		backend := newAllToolsBackend()
		response := invoke(t, strictServer(backend), strictContext("get_value"),
			toolCall("get_value", raw))
		require.Empty(t, response.Error.Code, "%s: %s", raw, response.Error.Message)
		require.Equal(t, 1, backend.calls(), raw)
	}
	backend := newAllToolsBackend()
	response := invoke(t, strictServer(backend), strictContext("get_ledger"),
		toolCall("get_ledger", `{"filter":null}`))
	require.Empty(t, response.Error.Code, response.Error.Message)
}

// caller_claims stays accepted where callers send it today and is still
// stripped before planning (removeCallerClaims, production_backend.go).
func TestStrictCallerClaimsStayAcceptedOnMutationIntents(t *testing.T) {
	for _, name := range []string{"optimize_query", "ensure_fk_indexes",
		"declare_table_contract", "register_consumer", "request_change",
		"propose_policy_change"} {
		backend := newAllToolsBackend()
		var object map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(strictArgs(backend, name)), &object))
		object["caller_claims"] = json.RawMessage(`{"role":"admin"}`)
		raw, err := json.Marshal(object)
		require.NoError(t, err)
		response := invoke(t, strictServer(backend), strictContext(name),
			toolCall(name, string(raw)))
		require.Empty(t, response.Error.Code, "%s: %s", name, response.Error.Message)
		require.Equal(t, 1, backend.calls(), name)
	}
}

// No concurrent-access test: argument validation is a pure function of
// the raw arguments and a read-only validator table.
// No integration test: no SQL or database behaviour changes here.
