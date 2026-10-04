package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Roadmap 2.3: MCP clients read facts, propose one (it stays proposed
// until a person confirms it) and confirm or reject one (operator or
// admin). Confirmed facts only narrow what pg_sage does.

type factRecordingBackend struct {
	recordingBackend
	calls  map[string]int
	last   FactRequest
	actor  string
	err    error
	result any
}

func (b *factRecordingBackend) record(name string, req FactRequest, actor string) (any,
	error) {
	if b.calls == nil {
		b.calls = map[string]int{}
	}
	b.calls[name]++
	b.last, b.actor = req, actor
	if b.result == nil {
		b.result = map[string]any{"ok": true}
	}
	return b.result, b.err
}

func (b *factRecordingBackend) ListFacts(_ context.Context, req FactRequest) (any, error) {
	return b.record("list", req, "")
}

func (b *factRecordingBackend) ProposeFact(_ context.Context, req FactRequest,
	actor string) (any, error) {
	return b.record("propose", req, actor)
}

func (b *factRecordingBackend) DecideFact(_ context.Context, req FactRequest,
	actor string) (any, error) {
	return b.record("decide", req, actor)
}

const (
	proposeFactArgs = `{"database":"orders","type":"test_fixture","subject_kind":"schema",` +
		`"subject":"test_*","evidence":"CI creates and leaks these"}`
	decideFactArgs = `{"database":"orders","fact_id":12,"decision":"confirm",` +
		`"note":"yes, CI fixtures"}`
)

func TestFactToolsAreListed(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range NewServer(&factRecordingBackend{}).Tools() {
		names[tool.Name] = true
	}
	for _, want := range []string{"list_facts", "propose_fact", "decide_fact"} {
		require.True(t, names[want], want)
	}
}

func TestFactToolsRoles(t *testing.T) {
	viewer := WithPrincipal(context.Background(), Principal{Actor: "user:3", Role: "viewer"})
	for name, args := range map[string]string{"propose_fact": proposeFactArgs,
		"decide_fact": decideFactArgs} {
		backend := &factRecordingBackend{}
		response := invoke(t, NewServer(backend), viewer, toolCall(name, args))
		require.Equal(t, -32001, response.Error.Code, name)
		require.Equal(t, 0, len(backend.calls), name)
	}
	backend := &factRecordingBackend{}
	response := invoke(t, NewServer(backend), viewer, toolCall("list_facts",
		`{"database":"orders","status":"proposed"}`))
	require.Empty(t, response.Error.Code)
	require.Equal(t, 1, backend.calls["list"])
	require.Equal(t, "proposed", backend.last.Status)
}

func TestFactToolsCarryTheActorAndArguments(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "alice", Role: "operator"})
	backend := &factRecordingBackend{}
	response := invoke(t, NewServer(backend), ctx, toolCall("propose_fact", proposeFactArgs))
	require.Empty(t, response.Error.Code)
	require.Equal(t, "mcp:alice", backend.actor)
	require.Equal(t, "test_*", backend.last.Subject)
	require.Equal(t, "CI creates and leaks these", backend.last.Evidence)
	response = invoke(t, NewServer(backend), ctx, toolCall("decide_fact", decideFactArgs))
	require.Empty(t, response.Error.Code)
	require.Equal(t, int64(12), backend.last.FactID)
	require.Equal(t, "confirm", backend.last.Decision)
}

func TestFactToolsValidateArguments(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "alice", Role: "admin"})
	for name, args := range map[string]string{
		"decide_fact":  `{"fact_id":0,"decision":"confirm"}`,
		"propose_fact": `{"type":"test_fixture","subject_kind":"schema"}`,
		"list_facts":   `{"fact_id":3}`,
	} {
		backend := &factRecordingBackend{}
		response := invoke(t, NewServer(backend), ctx, toolCall(name, args))
		require.Equal(t, -32602, response.Error.Code, name)
		require.Equal(t, 0, len(backend.calls), name)
	}
	backend := &factRecordingBackend{}
	response := invoke(t, NewServer(backend), ctx, toolCall("decide_fact",
		`{"fact_id":3,"decision":"maybe"}`))
	require.Equal(t, -32602, response.Error.Code)
}

func TestFactToolsMapErrors(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "alice", Role: "admin"})
	cases := map[error]int{
		facts.ErrNotFound:          -32004,
		facts.ErrProtectedSubject:  -32602,
		facts.ErrInvalidTransition: -32602,
		errors.New("boom"):         -32603,
	}
	for err, code := range cases {
		backend := &factRecordingBackend{err: err}
		response := invoke(t, NewServer(backend), ctx, toolCall("decide_fact", decideFactArgs))
		require.Equal(t, code, response.Error.Code, err.Error())
	}
	plain := invoke(t, NewServer(&recordingBackend{}), ctx, toolCall("list_facts", `{}`))
	require.Equal(t, -32603, plain.Error.Code)
}
