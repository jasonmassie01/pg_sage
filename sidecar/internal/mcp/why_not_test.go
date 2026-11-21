package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// why_not (ux-08b): ux-08a's chain over MCP, read-only, the same JSON as
// GET /api/v1/whynot. No DB here: the identity with the route on a real
// database is internal/api/whynot_mcp_db_test.go.
// No concurrency test: the tool keeps no state; each call is one read.

type whyNotRecordingBackend struct {
	recordingBackend
	calls    int
	last     WhyNotRequest
	database string
	result   any
	err      error
}

func (b *whyNotRecordingBackend) WhyNot(ctx context.Context,
	req WhyNotRequest) (any, error) {
	b.calls++
	b.last = req
	b.database, _ = DatabaseFromContext(ctx)
	if b.err != nil {
		return nil, b.err
	}
	if b.result != nil {
		return b.result, nil
	}
	return map[string]any{"database": req.Database, "links": []any{}, "failing": []any{}}, nil
}

func whyNotTool(t *testing.T) Tool {
	t.Helper()
	for _, tool := range NewServer(&whyNotRecordingBackend{}).Tools() {
		if tool.Name == toolWhyNot {
			return tool
		}
	}
	t.Fatal("why_not is not listed by tools/list")
	return Tool{}
}

func TestWhyNotToolIsListedReadOnlyWithItsArguments(t *testing.T) {
	tool := whyNotTool(t)
	require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, "read-only")
	for _, part := range []string{"Use when", "Returns", "Never"} {
		require.True(t, strings.Contains(tool.Description, part), part+": "+tool.Description)
	}
	require.True(t, len(tool.Description) <= 400,
		fmt.Sprintf("description is %d characters", len(tool.Description)))
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Additional bool                       `json:"additionalProperties"`
	}
	require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
	require.Equal(t, 4, len(schema.Properties), "database, finding_id, object, category")
	for _, name := range []string{"database", "finding_id", "object", "category"} {
		_, ok := schema.Properties[name]
		require.True(t, ok, name)
	}
	require.True(t, !schema.Additional, "additionalProperties false")
	require.True(t, strings.Contains(string(schema.Properties["finding_id"]), `"minimum":1`),
		"finding_id is a positive integer")
	scope, known := RequiredScope(toolWhyNot, nil)
	require.True(t, known && scope == ScopeRead, "read scope")
}

func TestWhyNotToolPassesTheQuestionAndTheBoundDatabase(t *testing.T) {
	backend := &whyNotRecordingBackend{}
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3", Role: "viewer"})
	server := NewServer(backend).WithDirectory(fleetOf("orders", "billing"))
	response := invoke(t, server, viewer, toolCall(toolWhyNot,
		`{"database":"orders","finding_id":17}`))
	require.Equal(t, 0, response.Error.Code, response.Error.Message)
	require.Equal(t, 1, backend.calls)
	require.Equal(t, "orders", backend.last.Database)
	require.Equal(t, "orders", backend.database)
	require.Equal(t, int64(17), backend.last.FindingID)
	require.Equal(t, "", backend.last.Object+backend.last.Category)
	response = invoke(t, server, viewer, toolCall(toolWhyNot,
		`{"database":"billing","object":"public.orders","category":"unused_index"}`))
	require.Equal(t, 0, response.Error.Code, response.Error.Message)
	require.Equal(t, "billing", backend.last.Database)
	require.Equal(t, int64(0), backend.last.FindingID)
	require.Equal(t, "public.orders", backend.last.Object)
	require.Equal(t, "unused_index", backend.last.Category)
}

// The result is the backend's JSON, unchanged, as structuredContent and
// as the text content.
func TestWhyNotToolReturnsTheChainAsStructuredContent(t *testing.T) {
	chain := map[string]any{"database": "orders", "first_fail": "policy",
		"failing": []any{"policy"}, "links": []any{map[string]any{"name": "policy",
			"status": "fail", "sentence": "Asks first: approval required.",
			"first_fail": true}}}
	backend := &whyNotRecordingBackend{result: chain}
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	response := invoke(t, server, context.Background(), toolCall(toolWhyNot, `{}`))
	require.Equal(t, 0, response.Error.Code, response.Error.Message)
	result, ok := response.Result.(map[string]any)
	require.True(t, ok, fmt.Sprintf("result %T", response.Result))
	want, _ := json.Marshal(chain)
	got, _ := json.Marshal(result["structuredContent"])
	require.Equal(t, string(want), string(got))
	content, _ := result["content"].([]any)
	require.Equal(t, 1, len(content))
	text, _ := content[0].(map[string]any)["text"].(string)
	require.Equal(t, string(want), text)
	require.Equal(t, "orders", backend.last.Database, "the only database is bound")
}

func TestWhyNotToolRefusesBadArgumentsBeforeTheBackend(t *testing.T) {
	backend := &whyNotRecordingBackend{}
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	for _, args := range []string{`{"finding_id":0}`, `{"finding_id":-3}`,
		`{"finding_id":"17"}`, `{"finding_id":1.5}`, `{"object":7}`, `{"category":[]}`,
		`{"other":1}`, `{"finding_id":17,"object":"public.t"}`,
		`{"finding_id":17,"category":"unused_index"}`,
		`{"object":"` + strings.Repeat("a", 200) + `"}`} {
		response := invoke(t, server, context.Background(), toolCall(toolWhyNot, args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
		require.True(t, strings.HasPrefix(response.Error.Message, "invalid arguments"),
			args+": "+response.Error.Message)
	}
	require.Equal(t, 0, backend.calls, "refusals never reach the backend")
	response := invoke(t, server, context.Background(), toolCall(toolWhyNot,
		`{"finding_id":1}`))
	require.Equal(t, 0, response.Error.Code, "finding_id 1 is the lower bound, inclusive")
}

func TestWhyNotToolScopesAndErrors(t *testing.T) {
	backend := &whyNotRecordingBackend{}
	server := NewServer(backend).WithDirectory(fleetOf("tenant_a", "tenant_b"))
	agent := WithPrincipal(context.Background(), Principal{Actor: "agent:a", Role: "viewer",
		Kind: KindAgent, Scopes: []Scope{ScopeRead}, Databases: []string{"tenant_a"}})
	response := invoke(t, server, agent, toolCall(toolWhyNot, `{"database":"tenant_a"}`))
	require.Equal(t, 0, response.Error.Code, "an agent with read scope may ask")
	require.Equal(t, "tenant_a", backend.last.Database)
	calls := backend.calls
	response = invoke(t, server, agent, toolCall(toolWhyNot, `{"database":"tenant_b"}`))
	require.True(t, response.Error.Code != 0, "a database outside the token is refused")
	require.Equal(t, calls, backend.calls, "the hidden database is never read")
	for _, c := range []struct {
		err  error
		code int
		msg  string
	}{
		{ErrWhyNotFindingNotFound, codeNotFound, "finding not found"},
		{ErrWhyNotDatabaseNotFound, codeNotFound, "database not found"},
		{fmt.Errorf("%w: object must be an unquoted schema.name", ErrWhyNotInvalidArguments),
			codeInvalidParams, "invalid arguments: object must be an unquoted schema.name"},
		{errors.New("read failed: host=10.0.0.1"), codeInternal, "internal error"},
		{context.DeadlineExceeded, codeCancelled, "request cancelled"},
	} {
		backend.err = c.err
		response = invoke(t, server, context.Background(), toolCall(toolWhyNot,
			`{"database":"tenant_a"}`))
		require.Equal(t, c.code, response.Error.Code, c.err.Error())
		require.Equal(t, c.msg, response.Error.Message)
	}
	response = invoke(t, NewServer(&recordingBackend{}).WithDirectory(fleetOf("tenant_a")),
		context.Background(), toolCall(toolWhyNot, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "no backend: unavailable")
	_, err := (&ProductionBackend{}).WhyNot(context.Background(), WhyNotRequest{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable), "no dependency")
}
