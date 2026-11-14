package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// apiauth-08c: when the HTTP transport cannot read a person's database
// grants, it binds the principal with DatabasesUnavailable and no
// databases. Every tool call and resource read is then the "unavailable"
// error (-32010), never a call over a widened list, and the backend is
// never reached. No concurrency test here: the check reads only the
// principal bound to the request context.

func unavailablePrincipal() context.Context {
	return WithPrincipal(context.Background(), Principal{Actor: "token:t1",
		Role: "operator", Kind: KindHuman, Databases: []string{}, OwnerUserID: 7,
		DatabasesUnavailable: true})
}

func TestDatabasesUnavailable_EveryToolCallIsUnavailable(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("billing", "search"))
	tools := map[string]string{
		"list_databases": `{}`,
		"fleet_findings": `{}`,
		"list_facts":     withDB(backend.validArgs("list_facts"), "billing"),
		"get_policy":     withDB(backend.validArgs("get_policy"), "billing"),
		"top_queries":    withDB(backend.validArgs("top_queries"), "billing"),
	}
	for name, args := range tools {
		response := invoke(t, server, unavailablePrincipal(), toolCall(name, args))
		require.Equal(t, -32010, response.Error.Code, name)
		require.Contains(t, response.Error.Message, "database grants", name)
	}
	require.Zero(t, backend.calls())
}

func TestDatabasesUnavailable_ResourceReadIsUnavailable(t *testing.T) {
	server, fake := briefingServer("orders", "billing")
	read := `{"jsonrpc":"2.0","id":1,"method":"resources/read",` +
		`"params":{"uri":"pgsage://databases/orders/briefings/latest"}}`
	response := invoke(t, server, unavailablePrincipal(), read)
	require.Equal(t, -32010, response.Error.Code)
	require.Empty(t, fake.asked)

	list := `{"jsonrpc":"2.0","id":2,"method":"resources/list","params":{}}`
	raw := server.Handle(unavailablePrincipal(), json.RawMessage(list))
	var out struct {
		Result struct {
			Resources []map[string]any `json:"resources"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, 0, len(out.Result.Resources), "resources: %s", raw)
}

// Boundary: an empty database list alone is "nothing permitted", not
// "unavailable"; only the flag turns calls into the unavailable error.
func TestDatabasesUnavailable_EmptyListWithoutTheFlagListsNothing(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("billing", "search"))
	ctx := WithPrincipal(context.Background(), Principal{Actor: "token:t2",
		Role: "operator", Kind: KindHuman, Databases: []string{}, OwnerUserID: 7})
	raw := server.Handle(ctx, json.RawMessage(toolCall("list_databases", `{}`)))
	var out struct {
		Result struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Databases []map[string]any `json:"databases"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.False(t, out.Result.IsError, "list_databases failed: %s", raw)
	require.Equal(t, 0, len(out.Result.StructuredContent.Databases), "listed: %s", raw)

	response := invoke(t, server, ctx, toolCall("list_facts",
		withDB(backend.validArgs("list_facts"), "billing")))
	require.Equal(t, -32003, response.Error.Code)
	require.Zero(t, backend.calls())
}
