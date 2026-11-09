package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Ask Sage over MCP (owner decision 5): ask_sage needs the read scope;
// whether the run may open an investigation or queue a proposal follows
// the caller's propose scope. An agent token can ask and propose, never
// approve: the tool has no approve path at all.

type askRecordingBackend struct {
	recordingBackend
	calls    int
	database string
	caller   ask.Caller
	request  ask.Request
	err      error
}

func (b *askRecordingBackend) AskSage(_ context.Context, database string, c ask.Caller,
	r ask.Request) (ask.Answer, error) {
	b.calls++
	b.database, b.caller, b.request = database, c, r
	if b.err != nil {
		return ask.Answer{}, b.err
	}
	return ask.Answer{ID: 9, Database: database, Status: ask.StatusNotObserved,
		Question: r.Question, Text: "I could not verify an answer."}, nil
}

const askArgs = `{"database":"orders","question":"Why is checkout slow?"}`

func TestAskToolIsListedWithReadScopeButNotReadOnly(t *testing.T) {
	var found *Tool
	for _, tool := range NewServer(&askRecordingBackend{}).Tools() {
		if tool.Name == "ask_sage" {
			tool := tool
			found = &tool
		}
	}
	require.True(t, found != nil, "ask_sage not listed")
	scope, ok := RequiredScope("ask_sage", nil)
	require.True(t, ok)
	require.Equal(t, ScopeRead, scope)
	require.True(t, found.Annotations != nil && !found.Annotations.ReadOnlyHint,
		"ask_sage may queue a proposal; it must not claim to be read-only")
}

func TestAskToolCallerFollowsThePrincipal(t *testing.T) {
	cases := []struct {
		name    string
		p       *Principal
		actor   string
		propose bool
	}{
		{"viewer", &Principal{Actor: "user:3", Role: "viewer"}, "mcp:user:3", false},
		{"operator", &Principal{Actor: "user:2", Role: "operator"}, "mcp:user:2", true},
		{"agent token", &Principal{Actor: "token:abc", Kind: KindAgent,
			Scopes: []Scope{ScopeRead, ScopePropose}}, "mcp:token:abc", true},
		{"read-only agent", &Principal{Actor: "token:ro", Kind: KindAgent,
			Scopes: []Scope{ScopeRead}}, "mcp:token:ro", false},
		{"agent claiming approve", &Principal{Actor: "token:x", Kind: KindAgent,
			Scopes: []Scope{ScopeRead, ScopePropose, ScopeApprove}}, "mcp:token:x", true},
		{"unbound library caller", nil, "mcp-agent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.p != nil {
				ctx = WithPrincipal(ctx, *tc.p)
			}
			backend := &askRecordingBackend{}
			response := invoke(t, NewServer(backend), ctx, toolCall("ask_sage", askArgs))
			require.Empty(t, response.Error.Code)
			require.Equal(t, 1, backend.calls)
			require.Equal(t, "orders", backend.database)
			require.Equal(t, "Why is checkout slow?", backend.request.Question)
			want := ask.Caller{Actor: tc.actor, MayPropose: tc.propose}
			require.Equal(t, want, backend.caller)
			content := structuredContent(t, response)
			require.Equal(t, ask.StatusNotObserved, content["status"])
		})
	}
}

func TestAskToolValidatesArguments(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "user:2", Role: "operator"})
	for _, args := range []string{`{"database":"orders"}`,
		`{"database":"orders","question":""}`,
		`{"database":"orders","question":"x","approve":true}`,
		`{"database":"orders","question":"x","conversation_id":7}`} {
		backend := &askRecordingBackend{}
		response := invoke(t, NewServer(backend), ctx, toolCall("ask_sage", args))
		require.Equal(t, -32602, response.Error.Code, args)
		require.Equal(t, 0, backend.calls, args)
	}
	backend := &askRecordingBackend{}
	response := invoke(t, NewServer(backend), ctx, toolCall("ask_sage",
		`{"database":"orders","question":"x","conversation_id":"c-1"}`))
	require.Empty(t, response.Error.Code)
	require.Equal(t, "c-1", backend.request.ConversationID)
}

func TestAskToolMapsErrors(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "user:2", Role: "operator"})
	cases := map[error]int{
		fmt.Errorf("%w: question too long", ask.ErrInvalid): -32602,
		fmt.Errorf("%w: conversation", ask.ErrNotFound):     -32004,
		ask.ErrDisabled:       -32010,
		errors.New("db down"): -32603,
	}
	for err, code := range cases {
		backend := &askRecordingBackend{err: err}
		response := invoke(t, NewServer(backend), ctx, toolCall("ask_sage", askArgs))
		require.Equal(t, code, response.Error.Code, err.Error())
		require.NotContains(t, response.Error.Message, "db down")
	}
	plain := invoke(t, NewServer(&recordingBackend{}), ctx, toolCall("ask_sage", askArgs))
	require.Equal(t, -32603, plain.Error.Code)
}

// Explain a case (mcp-01): read_only:true reaches Ask Sage, and even a
// propose-scoped token is passed on as a caller who may not propose, so
// no write tool is listed for that question. Omitting it changes nothing.
func TestAskToolReadOnlyDropsTheProposeRight(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "token:abc", Kind: KindAgent,
		Scopes: []Scope{ScopeRead, ScopePropose}})
	backend := &askRecordingBackend{}
	response := invoke(t, NewServer(backend), ctx, toolCall("ask_sage",
		`{"database":"orders","question":"Explain this case","read_only":true}`))
	require.Empty(t, response.Error.Code)
	require.Equal(t, 1, backend.calls)
	require.True(t, backend.request.ReadOnly, "read_only was not forwarded")
	require.Equal(t, ask.Caller{Actor: "mcp:token:abc", MayPropose: false},
		backend.caller)

	backend = &askRecordingBackend{}
	response = invoke(t, NewServer(backend), ctx, toolCall("ask_sage",
		`{"database":"orders","question":"Propose the fix","read_only":false}`))
	require.Empty(t, response.Error.Code)
	require.False(t, backend.request.ReadOnly)
	require.True(t, backend.caller.MayPropose, "read_only:false must keep the propose scope")
}

func TestAskToolSchemaDeclaresReadOnly(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		AdditionalProperties bool `json:"additionalProperties"`
	}
	for _, tool := range askTools() {
		require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
	}
	require.Equal(t, "boolean", schema.Properties["read_only"].Type)
	require.False(t, schema.AdditionalProperties)
}

func TestAskToolRejectsANonBooleanReadOnly(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "user:2", Role: "operator"})
	for _, args := range []string{
		`{"database":"orders","question":"x","read_only":"yes"}`,
		`{"database":"orders","question":"x","read_only":1}`,
	} {
		backend := &askRecordingBackend{}
		response := invoke(t, NewServer(backend), ctx, toolCall("ask_sage", args))
		require.Equal(t, -32602, response.Error.Code, args)
		require.Equal(t, 0, backend.calls, args)
	}
}
