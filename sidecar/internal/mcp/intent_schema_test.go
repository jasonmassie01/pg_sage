package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// request_change's intent is a oneOf over the kinds pg_sage can plan,
// each with an example; a shape matching no branch is refused before any
// backend sees it, naming the kinds (mcp-01a).

// No concurrent access tests: validation is a pure function of the call.
// No state transition tests: validating an intent changes nothing.

var plannableKinds = []string{"optimize_query", "ensure_fk_indexes", "apply_migration",
	"declare_table_contract", "register_consumer"}

func intentSchema(t *testing.T) map[string]any {
	t.Helper()
	schema := decodeSchema(t, toolIn(t, toolDefinitions(), "request_change").InputSchema)
	return objectMap(t, objectMap(t, schema["properties"])["intent"])
}

func intentBranches(t *testing.T) []any {
	t.Helper()
	branches, ok := intentSchema(t)["oneOf"].([]any)
	require.True(t, ok, "intent has a oneOf")
	return branches
}

func TestRequestChangeIntentIsAOneOfOverThePlannableKinds(t *testing.T) {
	branches := intentBranches(t)
	require.Len(t, branches, len(plannableKinds))
	require.Equal(t, plannableKinds, changeIntentKinds, "the kinds pg_sage plans")
	for i, kind := range plannableKinds {
		branch := objectMap(t, branches[i])
		require.Equal(t, kind, branch["title"])
		kindProperty := objectMap(t, objectMap(t, branch["properties"])["kind"])
		require.Equal(t, kind, kindProperty["const"])
		require.Equal(t, false, branch["additionalProperties"], kind)
		require.Contains(t, stringSlice(branch["required"]), "kind")
		examples, ok := branch["examples"].([]any)
		require.True(t, ok && len(examples) == 1, "%s has one example", kind)
		require.Equal(t, []int{i}, matchingBranches(branches, examples[0]),
			"%s's example matches its own branch only", kind)
	}
}

func TestRequestChangeIntentBranchesMirrorTheKindTools(t *testing.T) {
	tools := toolDefinitions()
	for i, kind := range plannableKinds {
		branch := objectMap(t, intentBranches(t)[i])
		own := objectMap(t, decodeSchema(t, toolIn(t, tools, kind).InputSchema)["properties"])
		properties := objectMap(t, branch["properties"])
		for name := range own {
			if name == "database" || name == "database_id" || name == "caller_claims" {
				continue
			}
			require.Contains(t, properties, name, "%s.%s", kind, name)
		}
		require.NotContains(t, properties, "database", "the call names the database")
	}
}

func changeServer(backend *recordingBackend) *Server {
	return NewServer(backend).WithDirectory(fleetOf("orders"))
}

func TestRequestChangeTypedIndexIntentReachesTheBackend(t *testing.T) {
	backend := &recordingBackend{changeResult: ChangeResult{Decision: "granted",
		EvidenceID: "ev-1", Reason: "allowed"}}
	response := invoke(t, changeServer(backend), operatorContext(context.Background()),
		toolCall("request_change",
			`{"intent":{"kind":"optimize_query","query_id":42,"goal":"latency"}}`))
	require.Zero(t, response.Error.Code, response.Error.Message)
	require.Equal(t, 1, backend.changeCalls)
	var intent map[string]any
	require.NoError(t, json.Unmarshal(backend.changeRequest.Intent, &intent))
	require.Equal(t, "optimize_query", intent["kind"])
	require.Equal(t, float64(42), intent["query_id"])
	content := structuredContent(t, response)
	require.Equal(t, "granted", content["decision"])
	require.Equal(t, "ev-1", content["evidence_id"])
}

func TestRequestChangeEveryKindExampleIsAccepted(t *testing.T) {
	for i, kind := range plannableKinds {
		example := objectMap(t, objectMap(t, intentBranches(t)[i])["examples"].([]any)[0])
		backend := &recordingBackend{changeResult: ChangeResult{Decision: "granted"}}
		response := invoke(t, changeServer(backend), adminContext(),
			toolCall("request_change", `{"intent":`+mustJSON(t, example)+`}`))
		require.Zero(t, response.Error.Code, "%s: %s", kind, response.Error.Message)
		require.Equal(t, 1, backend.changeCalls, kind)
	}
}

func refusedIntent(t *testing.T, ctx context.Context, intent string) map[string]any {
	t.Helper()
	backend := &recordingBackend{}
	response := invoke(t, changeServer(backend), ctx,
		toolCall("request_change", `{"intent":`+intent+`}`))
	require.Equal(t, codeInvalidParams, response.Error.Code, intent)
	require.Zero(t, backend.changeCalls, "%s reached the backend", intent)
	detail := errorDetail(t, response)
	require.Equal(t, "invalid_arguments", detail["reason"])
	return detail
}

func TestRequestChangeIntentMatchingNoBranchListsTheKinds(t *testing.T) {
	ctx := adminContext()
	for _, intent := range []string{`{"kind":"drop_table","table":"public.t"}`,
		`{"kind":7}`, `{"kind":"OPTIMIZE_QUERY"}`, `{"kind":""}`} {
		detail := refusedIntent(t, ctx, intent)
		message, _ := detail["message"].(string)
		for _, kind := range plannableKinds {
			require.Contains(t, message, kind, intent)
		}
		require.Equal(t, "request_change", objectMap(t, detail["next"])["tool"], intent)
	}
	unknown := refusedIntent(t, ctx, `{"kind":"drop_table"}`)
	require.Contains(t, unknown["message"], "unsupported mutation intent")
}

func TestRequestChangeIntentWithTheWrongArgumentsForItsKind(t *testing.T) {
	ctx := adminContext()
	for intent, want := range map[string]string{
		`{"kind":"ensure_fk_indexes","schema":"public","slot_name":"s"}`:  "ensure_fk_indexes",
		`{"kind":"optimize_query","query_id":"one"}`:                      "optimize_query",
		`{"kind":"register_consumer","slot_name":"s"}`:                    "owner",
		`{"kind":"apply_migration","table":"orders","sql":"ALTER TABLE"}`: "schema-qualified",
		`{"kind":"ensure_fk_indexes"}`:                                    "schema",
	} {
		detail := refusedIntent(t, ctx, intent)
		require.Contains(t, detail["message"], want, intent)
		next := objectMap(t, detail["next"])
		require.Equal(t, "request_change", next["tool"])
		example := objectMap(t, objectMap(t, next["example"])["intent"])
		var kind struct {
			Kind string `json:"kind"`
		}
		require.NoError(t, json.Unmarshal([]byte(intent), &kind))
		require.Equal(t, kind.Kind, example["kind"], "the example is of the kind asked for")
	}
}

func TestRequestChangeEmptyIntentNamesRequestChangeWithAnExample(t *testing.T) {
	for _, args := range []string{`{"intent":{}}`, `{}`, `{"intent":null}`} {
		backend := &recordingBackend{}
		response := invoke(t, changeServer(backend), operatorContext(context.Background()),
			toolCall("request_change", args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
		require.Zero(t, backend.changeCalls)
		detail := errorDetail(t, response)
		next := objectMap(t, detail["next"])
		require.Equal(t, "request_change", next["tool"], args)
		require.Equal(t, []any{"intent"}, next["required"], args)
		example := objectMap(t, next["example"])
		intent := objectMap(t, example["intent"])
		require.Contains(t, plannableKinds, intent["kind"], args)
		follow := invoke(t, changeServer(backend), adminContext(),
			toolCall("request_change", mustJSON(t, example)))
		require.Zero(t, follow.Error.Code, "the example is a valid call: %s",
			follow.Error.Message)
	}
}
