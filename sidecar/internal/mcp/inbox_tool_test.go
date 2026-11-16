package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// get_inbox (ux-01a): a read-only, fleet-wide tool that takes only
// database, kinds and limit. No DB here: the shared read is tested in
// internal/inbox and, against the HTTP route, in internal/api.

type inboxRecordingBackend struct {
	recordingBackend
	calls int
	last  InboxRequest
	err   error
}

func (b *inboxRecordingBackend) Inbox(_ context.Context, req InboxRequest) (any, error) {
	b.calls++
	b.last = req
	if b.err != nil {
		return nil, b.err
	}
	return map[string]any{"items": []any{}, "total": 0, "errors": []any{}}, nil
}

func inboxTool(t *testing.T) Tool {
	t.Helper()
	for _, tool := range NewServer(&inboxRecordingBackend{}).Tools() {
		if tool.Name == toolGetInbox {
			return tool
		}
	}
	t.Fatal("get_inbox is not listed by tools/list")
	return Tool{}
}

func TestInboxToolIsListedReadOnlyAndNeverApproves(t *testing.T) {
	tool := inboxTool(t)
	require.True(t, tool.Annotations != nil && tool.Annotations.ReadOnlyHint, "read-only")
	require.True(t, strings.Contains(tool.Description, "never approves"), tool.Description)
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Additional bool                       `json:"additionalProperties"`
	}
	require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
	require.Equal(t, 3, len(schema.Properties), "database, kinds and limit only")
	for _, name := range []string{"database", "kinds", "limit"} {
		_, ok := schema.Properties[name]
		require.True(t, ok, name)
	}
	require.True(t, !schema.Additional, "additionalProperties false")
	require.True(t, strings.Contains(string(schema.Properties["kinds"]), `"policy_exception"`),
		"kinds enumerates the seven kinds")
	scope, known := RequiredScope(toolGetInbox, nil)
	require.True(t, known && scope == ScopeRead, "read scope")
}

func TestInboxToolPassesArgumentsAndAViewerMayRead(t *testing.T) {
	backend := &inboxRecordingBackend{}
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3", Role: "viewer"})
	response := invoke(t, NewServer(backend), viewer, toolCall(toolGetInbox,
		`{"database":"orders","kinds":["fact","change_approval"],"limit":5}`))
	require.Equal(t, 0, response.Error.Code, "viewer may read the inbox")
	require.Equal(t, 1, backend.calls)
	require.Equal(t, "orders", backend.last.Database)
	require.Equal(t, "fact,change_approval", strings.Join(backend.last.Kinds, ","))
	require.Equal(t, 5, backend.last.Limit)
	require.True(t, backend.last.Databases == nil, "unrestricted principal: every database")
}

func TestInboxToolRestrictsScopedPrincipalsToTheirDatabases(t *testing.T) {
	backend := &inboxRecordingBackend{}
	agent := WithPrincipal(context.Background(), Principal{Actor: "agent:a", Role: "viewer",
		Kind: KindAgent, Databases: []string{"tenant_a"}})
	response := invoke(t, NewServer(backend), agent, toolCall(toolGetInbox, `{}`))
	require.Equal(t, 0, response.Error.Code, "an agent may read")
	require.Equal(t, "tenant_a", strings.Join(backend.last.Databases, ","))
	require.Equal(t, 0, backend.last.Limit)
}

func TestInboxToolRefusesAnyOtherArgument(t *testing.T) {
	backend := &inboxRecordingBackend{}
	for _, args := range []string{`{"other":1}`, `{"approve":true}`, `{"limit":0}`,
		`{"limit":-1}`, `{"limit":10001}`, `{"limit":"5"}`, `{"kinds":["facts"]}`,
		`{"kinds":"fact"}`, `{"database":"../x"}`, `{"database":7}`} {
		response := invoke(t, NewServer(backend), context.Background(),
			toolCall(toolGetInbox, args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	require.Equal(t, 0, backend.calls)
	response := invoke(t, NewServer(backend), context.Background(),
		toolCall(toolGetInbox, `{"limit":10000,"kinds":[]}`))
	require.Equal(t, 0, response.Error.Code, "limit 10000 is the bound, inclusive")
	require.Equal(t, 10000, backend.last.Limit)
}

func TestInboxToolErrorsAreDistinguishable(t *testing.T) {
	backend := &inboxRecordingBackend{err: ErrInboxDatabaseNotFound}
	response := invoke(t, NewServer(backend), context.Background(),
		toolCall(toolGetInbox, `{"database":"other"}`))
	require.Equal(t, codeNotFound, response.Error.Code, "unknown or hidden database")
	backend.err = errors.New("fan-out failed")
	response = invoke(t, NewServer(backend), context.Background(),
		toolCall(toolGetInbox, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "backend error surfaces")
	response = invoke(t, NewServer(&recordingBackend{}), context.Background(),
		toolCall(toolGetInbox, `{}`))
	require.Equal(t, codeInternal, response.Error.Code, "no backend: unavailable")
	require.True(t, strings.Contains(response.Error.Message, "inbox"), response.Error.Message)
}

func TestInboxToolProductionBackendNeedsItsDependency(t *testing.T) {
	backend := &ProductionBackend{}
	_, err := backend.Inbox(context.Background(), InboxRequest{})
	require.True(t, errors.Is(err, ErrProductionDependencyUnavailable), "no dependency")
	rec := &inboxRecordingBackend{}
	backend = &ProductionBackend{dependencies: ProductionDependencies{Inbox: rec}}
	got, err := backend.Inbox(context.Background(), InboxRequest{Database: "orders"})
	require.NoError(t, err)
	require.Equal(t, "orders", rec.last.Database)
	require.True(t, got.(map[string]any)["total"] == 0, "result passed through")
}
