package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// mcp-09: tools/list shows a tool only where it can be served. A backend
// that implements Availability says, per family and database, whether
// Ask Sage, the specialist contract or a required extension is there;
// a backend without it lists exactly what it listed before.

// availabilityBackend is allToolsBackend with switchable availability.
// Unknown keys are available, the way production treats unknown evidence.
type availabilityBackend struct {
	*allToolsBackend
	mu     sync.Mutex
	off    map[string]bool // family+"/"+database
	checks int
}

func newAvailabilityBackend() *availabilityBackend {
	return &availabilityBackend{allToolsBackend: newAllToolsBackend(), off: map[string]bool{}}
}

func (b *availabilityBackend) Available(_ context.Context, family, database string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.checks++
	return !b.off[family+"/"+database]
}

func (b *availabilityBackend) set(family, database string, available bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.off[family+"/"+database] = !available
}

func (b *availabilityBackend) checkCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.checks
}

// AskSage answers like a registry whose Ask Sage is switched off.
func (b *availabilityBackend) AskSage(context.Context, string, ask.Caller, ask.Request) (
	ask.Answer, error) {
	return ask.Answer{}, ask.ErrDisabled
}

var adminCtx = WithPrincipal(context.Background(), Principal{Actor: "user:1", Role: "admin"})

// listedSchemas is tools/list for ctx as name -> tool object.
func listedSchemas(t *testing.T, server *Server, ctx context.Context) map[string]map[string]any {
	t.Helper()
	r := invoke(t, server, ctx, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	require.Equal(t, 0, r.Error.Code, r.Error.Message)
	out := map[string]map[string]any{}
	for _, item := range objectMap(t, r.Result)["tools"].([]any) {
		tool := objectMap(t, item)
		out[tool["name"].(string)] = tool
	}
	return out
}

func databaseEnum(t *testing.T, tool map[string]any) []string {
	t.Helper()
	props := objectMap(t, objectMap(t, tool["inputSchema"])["properties"])
	return stringSlice(objectMap(t, props["database"])["enum"])
}

func specialistNames() []string {
	var names []string
	for _, tool := range specialistTools() {
		names = append(names, tool.Name)
	}
	return names
}

func TestToolsListHidesAskWhenAskIsUnavailable(t *testing.T) {
	backend := newAvailabilityBackend()
	server := NewServer(backend)
	require.Contains(t, listedSchemas(t, server, adminCtx), "ask_sage")
	backend.set(FamilyAsk, "", false)
	listed := listedSchemas(t, server, adminCtx)
	require.NotContains(t, listed, "ask_sage")
	require.Contains(t, listed, "list_facts", "an ungated tool stays listed")
}

func TestToolsListShowsSpecialistToolsOnlyWhenTheContractIsOn(t *testing.T) {
	backend := newAvailabilityBackend()
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	backend.set(FamilySpecialist, "orders", false)
	off := listedSchemas(t, server, adminCtx)
	for _, name := range specialistNames() {
		require.NotContains(t, off, name)
	}
	backend.set(FamilySpecialist, "orders", true)
	on := listedSchemas(t, server, adminCtx)
	require.Len(t, specialistNames(), 5)
	for _, name := range specialistNames() {
		require.Contains(t, on, name)
	}
}

// Boundary (fleet): one of two databases lacks pg_stat_statements, so
// top_queries is listed with only the other database to name, while an
// ungated tool keeps both.
func TestToolsListNarrowsTheDatabaseEnumToWhereTheToolWorks(t *testing.T) {
	backend := newAvailabilityBackend()
	server := NewServer(backend).WithDirectory(fleetOf("orders", "billing"))
	backend.set(ExtensionFamily("pg_stat_statements"), "billing", false)
	listed := listedSchemas(t, server, adminCtx)
	require.Equal(t, []string{"orders"}, databaseEnum(t, listed["top_queries"]))
	require.Equal(t, []string{"billing", "orders"}, databaseEnum(t, listed["explain_query"]))
	require.Equal(t, []string{"billing", "orders"}, databaseEnum(t, listed["query_sources"]),
		"query_sources samples pg_stat_activity and needs no extension")
	require.Equal(t, []string{"orders"}, databaseEnum(t, listed["whatif_index"]),
		"whatif_index measures pg_stat_statements query ids")
	backend.set(ExtensionFamily("pg_stat_statements"), "orders", false)
	require.NotContains(t, listedSchemas(t, server, adminCtx), "top_queries",
		"no permitted database can serve it")
}

// The enum narrows within what the caller may name, never beyond it.
func TestToolsListNarrowsWithinThePrincipalsDatabases(t *testing.T) {
	backend := newAvailabilityBackend()
	server := NewServer(backend).WithDirectory(fleetOf("orders", "billing", "zeta"))
	backend.set(ExtensionFamily("pg_stat_statements"), "orders", false)
	listed := listedSchemas(t, server, restricted("orders", "billing"))
	require.Equal(t, []string{"billing"}, databaseEnum(t, listed["top_queries"]))
	onlyOrders := listedSchemas(t, server, restricted("orders"))
	require.NotContains(t, onlyOrders, "top_queries")
	require.Contains(t, onlyOrders, "explain_query")
}

// Error propagation: a hidden tool called directly is not "unknown"; the
// call path still answers with the typed unavailable code.
func TestHiddenToolCalledDirectlyIsUnavailableNotUnknown(t *testing.T) {
	backend := newAvailabilityBackend()
	backend.set(FamilyAsk, "", false)
	backend.set(ExtensionFamily("pg_stat_statements"), "", false)
	backend.agentErr = fmt.Errorf("%w: pg_stat_statements is not usable here",
		agenttools.ErrUnavailable)
	server := NewServer(backend)
	listed := listedSchemas(t, server, adminCtx)
	require.NotContains(t, listed, "ask_sage")
	require.NotContains(t, listed, "top_queries")
	r := invoke(t, server, adminCtx, toolCall("ask_sage", `{"question":"why slow?"}`))
	require.Equal(t, codeUnavailable, r.Error.Code, r.Error.Message)
	require.Contains(t, r.Error.Message, "disabled")
	r = invoke(t, server, adminCtx, toolCall("top_queries", `{}`))
	require.Equal(t, codeUnavailable, r.Error.Code, r.Error.Message)
	require.NotContains(t, r.Error.Message, "Unknown tool")
}

// The scope filter still applies before availability: a viewer is not
// shown an available tool it may not call.
func TestAvailabilityDoesNotWidenTheScopeFilter(t *testing.T) {
	server := NewServer(newAvailabilityBackend())
	viewer := listedSchemas(t, server, sessionContext("viewer"))
	require.NotContains(t, viewer, "explain_query")
	require.NotContains(t, viewer, "request_change")
}

// legacyToolsList is tools/list as it was before availability existed.
func legacyToolsList(s *Server, ctx context.Context) []Tool {
	names := s.permittedDatabaseNames(ctx)
	var tools []Tool
	for _, tool := range s.tools {
		if mayCall(ctx, scopeFor(ctx, tool.Name, nil)) {
			tools = append(tools, withDatabaseEnum(tool, names))
		}
	}
	return tools
}

func legacyFingerprint(s *Server) string {
	var names []string
	if s.directory != nil {
		names = []string{}
		for _, ref := range s.directory.Databases() {
			names = append(names, ref.Name)
		}
	}
	tools := make([]Tool, 0, len(s.tools))
	for _, tool := range s.tools {
		tools = append(tools, withDatabaseEnum(tool, sortedCopy(names)))
	}
	raw, _ := json.Marshal(tools)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Nil/empty: a backend without Available lists exactly today's set, and
// its fingerprint is unchanged (no list_changed on upgrade).
func TestBackendWithoutAvailabilityListsTodaysSet(t *testing.T) {
	for _, dir := range []*fakeDirectory{nil, fleetOf(), fleetOf("orders", "billing")} {
		server := newAllToolsBackend().server()
		if dir != nil {
			server = server.WithDirectory(dir)
		}
		for _, ctx := range []context.Context{adminCtx, viewerCtx, context.Background(),
			restricted("billing")} {
			r := invoke(t, server, ctx, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			got, err := json.Marshal(objectMap(t, r.Result)["tools"])
			require.NoError(t, err)
			want, err := json.Marshal(legacyToolsList(server, ctx))
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(got))
		}
		require.Equal(t, legacyFingerprint(server), server.Fingerprint())
	}
	all := listedSchemas(t, newAllToolsBackend().server(), adminCtx)
	require.Len(t, all, len(toolDefinitions()), "an admin sees every tool")
	require.Contains(t, all, "ask_sage")
	require.Contains(t, all, "specialist_open_investigation")
	require.Contains(t, all, "top_queries")
}

// A backend that reports everything available lists the same set as one
// without availability.
func TestEverythingAvailableListsTheSameSet(t *testing.T) {
	dir := fleetOf("orders", "billing")
	with := NewServer(newAvailabilityBackend()).WithDirectory(dir)
	without := newAllToolsBackend().server().WithDirectory(dir)
	require.Equal(t, without.Fingerprint(), with.Fingerprint())
	require.Equal(t, len(listedSchemas(t, without, adminCtx)),
		len(listedSchemas(t, with, adminCtx)))
}

// Ungated tools never ask the backend: only the gated families are read.
func TestOnlyGatedToolsAreChecked(t *testing.T) {
	backend := newAvailabilityBackend()
	server := NewServer(backend).WithDirectory(fleetOf("orders", "billing"))
	listedSchemas(t, server, adminCtx)
	gated := 0
	for _, tool := range toolDefinitions() {
		if availabilityFamily(tool.Name) != "" {
			gated++
		}
	}
	require.Equal(t, 8, gated, "ask_sage, five specialist tools, top_queries, whatif_index")
	require.Equal(t, gated*2, backend.checkCount(), "one check per gated tool and database")
}

func TestAvailabilityFamilies(t *testing.T) {
	require.Equal(t, FamilyAsk, availabilityFamily("ask_sage"))
	require.Equal(t, FamilySpecialist, availabilityFamily("specialist_investigation_status"))
	require.Equal(t, ExtensionFamily("pg_stat_statements"), availabilityFamily("top_queries"))
	require.Equal(t, ExtensionFamily("pg_stat_statements"), availabilityFamily("whatif_index"))
	for _, name := range []string{"query_sources", "explain_query", "lint_migration",
		"list_databases", "get_policy", "nope", ""} {
		require.Equal(t, "", availabilityFamily(name), name)
	}
	ext, ok := FamilyExtension(ExtensionFamily("hypopg"))
	require.True(t, ok)
	require.Equal(t, "hypopg", ext)
	_, ok = FamilyExtension(FamilyAsk)
	require.False(t, ok)
	_, ok = FamilyExtension("extension:")
	require.False(t, ok, "an empty extension name is not an extension family")
}

// The fingerprint follows availability, so the watcher can tell clients.
func TestFingerprintChangesWithAvailability(t *testing.T) {
	backend := newAvailabilityBackend()
	server := NewServer(backend).WithDirectory(fleetOf("orders", "billing"))
	base := server.Fingerprint()
	backend.set(ExtensionFamily("pg_stat_statements"), "billing", false)
	narrowed := server.Fingerprint()
	require.NotEqual(t, base, narrowed, "a narrowed enum is a different list")
	backend.set(FamilyAsk, "orders", false)
	backend.set(FamilyAsk, "billing", false)
	hidden := server.Fingerprint()
	require.NotEqual(t, narrowed, hidden, "a hidden tool is a different list")
	backend.set(FamilyAsk, "orders", true)
	backend.set(FamilyAsk, "billing", true)
	backend.set(ExtensionFamily("pg_stat_statements"), "billing", true)
	require.Equal(t, base, server.Fingerprint(), "restored availability restores the list")
}

// Concurrency: tools/list and Fingerprint run while availability flips.
// -race is unavailable on this machine (no cgo); the test still proves
// every list is one of the two consistent states.
func TestToolsListAndFingerprintWhileAvailabilityFlips(t *testing.T) {
	backend := newAvailabilityBackend()
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	on := server.Fingerprint()
	backend.set(FamilyAsk, "orders", false)
	off := server.Fingerprint()
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				backend.set(FamilyAsk, "orders", (i+j)%2 == 0)
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if f := server.Fingerprint(); f != on && f != off {
					errs <- "fingerprint of neither state"
					return
				}
				raw := server.Handle(adminCtx,
					json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
				if !strings.Contains(string(raw), `"list_facts"`) {
					errs <- "tools/list lost an ungated tool"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

// The generated reference keeps every tool and states when each is listed.
func TestToolReferenceStatesAvailability(t *testing.T) {
	tools := NewServer(&recordingBackend{}).Tools()
	text := ToolReferenceMarkdown(tools)
	require.Contains(t, text, "| Tool | Scope | Available when |")
	for _, tool := range tools {
		scope, _ := RequiredScope(tool.Name, nil)
		row := fmt.Sprintf("| [`%s`](#%s) | `%s` | %s |", tool.Name, tool.Name, scope,
			AvailableWhen(tool.Name))
		require.Contains(t, text, row, tool.Name)
	}
	require.Equal(t, "always", AvailableWhen("get_policy"))
	require.Contains(t, AvailableWhen("ask_sage"), "ask.enabled")
	require.Contains(t, AvailableWhen("top_queries"), "pg_stat_statements")
	require.Contains(t, AvailableWhen("specialist_request_remediation"), "specialist")
}
