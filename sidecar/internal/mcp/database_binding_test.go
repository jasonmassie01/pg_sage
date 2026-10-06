package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Fleet mode: every tool takes a `database` argument, validated against
// the fleet; it defaults only when exactly one database is monitored. A
// principal restricted to some databases is refused any other, with an
// error that does not reveal whether that database exists.

type fakeDirectory struct {
	mu   sync.Mutex
	refs []DatabaseRef
}

func (d *fakeDirectory) Databases() []DatabaseRef {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DatabaseRef(nil), d.refs...)
}

func (d *fakeDirectory) set(refs ...DatabaseRef) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refs = refs
}

func fleetOf(names ...string) *fakeDirectory {
	d := &fakeDirectory{}
	for i, name := range names {
		d.refs = append(d.refs, DatabaseRef{Name: name, ID: int64(i + 1)})
	}
	return d
}

func restricted(dbs ...string) context.Context {
	return WithPrincipal(context.Background(), Principal{Actor: "token:r1",
		Kind: KindAgent, Scopes: []Scope{ScopeRead, ScopePropose}, Databases: dbs})
}

func withDB(args, db string) string {
	var object map[string]any
	_ = json.Unmarshal([]byte(args), &object)
	object["database"] = db
	raw, _ := json.Marshal(object)
	return string(raw)
}

func TestFleetModeRequiresADatabase(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("orders", "billing"))
	for _, name := range []string{"list_facts", "top_queries", "get_policy", "get_value",
		"sre_list_incidents", "get_guarantee_status"} {
		response := invoke(t, server, operatorContext(context.Background()),
			toolCall(name, backend.validArgs(name)))
		require.Equal(t, -32006, response.Error.Code, name)
		require.Contains(t, response.Error.Message, "orders", name)
	}
	require.Zero(t, backend.calls())
}

func TestFleetModeRoutesTheNamedDatabase(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("orders", "billing"))
	response := invoke(t, server, operatorContext(context.Background()),
		toolCall("list_facts", `{"database":"billing"}`))
	require.Empty(t, response.Error.Code)
	require.Equal(t, "billing", backend.facts.req.Database)
	require.Equal(t, "billing", backend.ctxDB)
}

func TestUnknownDatabaseIsRefused(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("orders"))
	for _, name := range []string{"nosuch", "orders'; DROP DATABASE orders; --", "ORDERS",
		" orders", "orders\n"} {
		response := invoke(t, server, operatorContext(context.Background()),
			toolCall("list_facts", `{"database":`+mustJSON(t, name)+`}`))
		require.Equal(t, -32007, response.Error.Code, name)
	}
	require.Zero(t, backend.calls())
}

func TestSingleDatabaseModeDefaults(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("only"))
	response := invoke(t, server, viewerCtx, toolCall("list_facts", `{}`))
	require.Empty(t, response.Error.Code)
	require.Equal(t, "only", backend.facts.req.Database)
	require.Equal(t, "only", backend.ctxDB)
}

func TestEmptyFleetIsUnknownDatabase(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf())
	response := invoke(t, server, viewerCtx, toolCall("list_facts", `{}`))
	require.Equal(t, -32006, response.Error.Code)
	named := invoke(t, server, viewerCtx, toolCall("list_facts", `{"database":"x"}`))
	require.Equal(t, -32007, named.Error.Code)
	require.Zero(t, backend.calls())
}

func TestRestrictedPrincipalCannotUseOtherDatabases(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("orders", "billing"))
	ctx := restricted("orders")
	for _, db := range []string{"billing", "nosuch"} {
		for _, name := range []string{"list_facts", "top_queries", "propose_fact",
			"get_source_fix_packet", "report_source_fix", "get_policy"} {
			response := invoke(t, server, ctx, toolCall(name,
				withDB(backend.validArgs(name), db)))
			require.Equal(t, -32003, response.Error.Code, "%s on %s", name, db)
		}
	}
	require.Zero(t, backend.calls())
	ok := invoke(t, server, ctx, toolCall("list_facts", `{"database":"orders"}`))
	require.Empty(t, ok.Error.Code)
	require.Equal(t, 1, backend.calls())
}

func TestRestrictedPrincipalInSingleDatabaseMode(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("orders"))
	response := invoke(t, server, restricted("billing"), toolCall("list_facts", `{}`))
	require.Equal(t, -32003, response.Error.Code)
	allowed := invoke(t, server, restricted("orders"), toolCall("list_facts", `{}`))
	require.Empty(t, allowed.Error.Code)
}

func TestRestrictedPrincipalWithoutDirectoryMustName(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server()
	missing := invoke(t, server, restricted("orders"), toolCall("list_facts", `{}`))
	require.Equal(t, -32006, missing.Error.Code)
	other := invoke(t, server, restricted("orders"),
		toolCall("list_facts", `{"database":"billing"}`))
	require.Equal(t, -32003, other.Error.Code)
	require.Zero(t, backend.calls())
}

func TestLegacyDatabaseIDResolvesAndMustAgree(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("orders", "billing"))
	ctx := operatorContext(context.Background())
	byID := invoke(t, server, ctx, toolCall("optimize_query",
		`{"database_id":2,"goal":"latency","query_id":1}`))
	require.Empty(t, byID.Error.Code)
	require.Equal(t, "billing", backend.ctxDB)
	var forwarded map[string]any
	require.NoError(t, json.Unmarshal(backend.intent, &forwarded))
	require.Equal(t, "billing", forwarded["database"])
	require.Equal(t, float64(2), forwarded["database_id"])
	conflict := invoke(t, server, ctx, toolCall("optimize_query",
		`{"database":"orders","database_id":2,"goal":"latency","query_id":1}`))
	require.Equal(t, -32602, conflict.Error.Code)
	unknownID := invoke(t, server, ctx, toolCall("optimize_query",
		`{"database_id":99,"goal":"latency","query_id":1}`))
	require.Equal(t, -32007, unknownID.Error.Code)
	require.Equal(t, 1, backend.calls())
}

func TestDatabaseMustBeAString(t *testing.T) {
	backend := newAllToolsBackend()
	server := backend.server().WithDirectory(fleetOf("orders"))
	for _, value := range []string{`7`, `["orders"]`, `{"name":"orders"}`, `true`} {
		response := invoke(t, server, viewerCtx,
			toolCall("list_facts", `{"database":`+value+`}`))
		require.Equal(t, -32602, response.Error.Code, value)
	}
	require.Zero(t, backend.calls())
}

func TestEveryToolSchemaTakesADatabase(t *testing.T) {
	for _, tool := range NewServer(&recordingBackend{}).Tools() {
		properties := objectMap(t, decodeSchema(t, tool.InputSchema)["properties"])
		// Fleet-wide tools (list_databases, fleet_findings) name no database.
		if fleetWideTool(tool.Name) {
			require.NotContains(t, properties, "database")
			continue
		}
		db := objectMap(t, properties["database"])
		require.Equal(t, "string", db["type"], tool.Name)
		require.NotContains(t, stringSlice(decodeSchema(t, tool.InputSchema)["required"]),
			"database", tool.Name)
	}
}

func TestToolsListEnumeratesPermittedDatabases(t *testing.T) {
	server := newAllToolsBackend().server().WithDirectory(
		fleetOf("zeta", "alpha", "billing"))
	enumOf := func(ctx context.Context) []string {
		response := invoke(t, server, ctx, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		for _, item := range objectMap(t, response.Result)["tools"].([]any) {
			tool := objectMap(t, item)
			if tool["name"] != "list_facts" {
				continue
			}
			props := objectMap(t, objectMap(t, tool["inputSchema"])["properties"])
			return stringSlice(objectMap(t, props["database"])["enum"])
		}
		t.Fatal("list_facts not listed")
		return nil
	}
	require.Equal(t, []string{"alpha", "billing", "zeta"}, enumOf(viewerCtx))
	require.Equal(t, []string{"billing"}, enumOf(restricted("billing", "gone")))
}

func TestListDatabasesShowsOnlyPermitted(t *testing.T) {
	server := newAllToolsBackend().server().WithDirectory(fleetOf("orders", "billing"))
	all := structuredContent(t, invoke(t, server, viewerCtx,
		toolCall("list_databases", `{}`)))
	require.Len(t, all["databases"].([]any), 2)
	require.Equal(t, false, all["single_database_mode"])
	some := structuredContent(t, invoke(t, server, restricted("billing"),
		toolCall("list_databases", `{}`)))
	databases := some["databases"].([]any)
	require.Len(t, databases, 1)
	require.Equal(t, "billing", objectMap(t, databases[0])["name"])
	bad := invoke(t, server, viewerCtx, toolCall("list_databases", `{"database":"x"}`))
	require.Equal(t, -32602, bad.Error.Code)
}

func TestDatabaseContextHelpers(t *testing.T) {
	_, ok := DatabaseFromContext(context.Background())
	require.False(t, ok)
	name, ok := DatabaseFromContext(WithDatabase(context.Background(), "orders"))
	require.True(t, ok)
	require.Equal(t, "orders", name)
	_, ok = DatabaseFromContext(WithDatabase(context.Background(), ""))
	require.False(t, ok)
}

func TestMayUseDatabase(t *testing.T) {
	require.True(t, Principal{Actor: "u"}.MayUseDatabase("x"))
	p := Principal{Actor: "u", Databases: []string{"a", "b"}}
	require.True(t, p.MayUseDatabase("b"))
	require.False(t, p.MayUseDatabase("c"))
	require.False(t, p.MayUseDatabase(""))
	require.False(t, Principal{Actor: "u", Databases: []string{}}.MayUseDatabase("a"))
}
