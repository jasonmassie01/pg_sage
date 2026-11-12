package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// llm-06b: the briefing MCP resource. resources/list names one latest
// briefing per database the caller may use; resources/read returns the
// stored briefing (content_json unchanged) and needs the read scope.

const briefingRecordJSON = `{"id":9,"database":"orders","generated_at":"2026-11-09T06:00:00Z",` +
	`"content_text":"# Briefing","content_json":{"total_open":4,"window":{"start":` +
	`"2026-11-08T06:00:00Z","end":"2026-11-09T06:00:00Z"},"open_by_severity":` +
	`{"critical":1,"warning":3}},"delivery_status":null}`

type briefingFake struct {
	recordingBackend
	mu      sync.Mutex
	records map[string]string
	err     error
	asked   []string
}

func (f *briefingFake) LatestBriefing(ctx context.Context, database string) (
	json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, database)
	if bound, _ := DatabaseFromContext(ctx); bound != database {
		return nil, fmt.Errorf("context database %q != %q", bound, database)
	}
	if f.err != nil {
		return nil, f.err
	}
	record, ok := f.records[database]
	if !ok {
		return nil, fmt.Errorf("%w: briefing.schedule says when", ErrNoBriefing)
	}
	return json.RawMessage(record), nil
}

func briefingServer(names ...string) (*Server, *briefingFake) {
	fake := &briefingFake{records: map[string]string{"orders": briefingRecordJSON}}
	server := NewServer(fake)
	if len(names) > 0 {
		server = server.WithDirectory(fleetOf(names...))
	}
	return server, fake
}

func readRequest(uri string) string {
	return `{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"` + uri + `"}}`
}

const listRequest = `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`

func resourceURIs(t *testing.T, response protocolResponse) []string {
	t.Helper()
	require.Empty(t, response.Error.Code, response.Error.Message)
	items, ok := objectMap(t, response.Result)["resources"].([]any)
	require.True(t, ok, "resources is not an array: %#v", response.Result)
	out := []string{}
	for _, item := range items {
		resource := objectMap(t, item)
		require.Equal(t, "application/json", resource["mimeType"])
		require.NotEmpty(t, resource["name"])
		require.NotEmpty(t, resource["description"])
		out = append(out, resource["uri"].(string))
	}
	return out
}

func briefingAgent(scopes []Scope, databases ...string) context.Context {
	var allowed []string
	if len(databases) > 0 {
		allowed = databases
	}
	return WithPrincipal(context.Background(), Principal{Actor: "token:a1", Kind: KindAgent,
		Scopes: scopes, Databases: allowed})
}

func TestResourcesAreAKnownMethodAndCapability(t *testing.T) {
	require.True(t, KnownMethod("resources/list"))
	require.True(t, KnownMethod("resources/read"))
	server, _ := briefingServer("orders")
	response := invoke(t, server, context.Background(), `{"jsonrpc":"2.0","id":1,`+
		`"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`)
	capabilities := objectMap(t, objectMap(t, response.Result)["capabilities"])
	require.Contains(t, capabilities, "resources")
	require.Contains(t, capabilities, "tools")
}

// Happy path: one latest-briefing resource per database, sorted.
func TestResourcesListOnePerPermittedDatabase(t *testing.T) {
	server, _ := briefingServer("orders", "billing")
	uris := resourceURIs(t, invoke(t, server, operatorContext(context.Background()),
		listRequest))
	require.Equal(t, []string{"pgsage://databases/billing/briefings/latest",
		"pgsage://databases/orders/briefings/latest"}, uris)
	restrictedAgent := briefingAgent([]Scope{ScopeRead}, "orders")
	uris = resourceURIs(t, invoke(t, server, restrictedAgent, listRequest))
	require.Equal(t, []string{"pgsage://databases/orders/briefings/latest"}, uris)
}

// Nil/empty: with no directory the default database's resource is listed.
func TestResourcesListWithoutDirectory(t *testing.T) {
	server, _ := briefingServer()
	uris := resourceURIs(t, invoke(t, server, context.Background(), listRequest))
	require.Equal(t, []string{"pgsage://briefings/latest"}, uris)
}

// Scope: an agent without the read scope sees no resources and may not read.
func TestResourcesNeedTheReadScope(t *testing.T) {
	server, fake := briefingServer("orders")
	noRead := briefingAgent([]Scope{ScopePropose})
	require.Empty(t, resourceURIs(t, invoke(t, server, noRead, listRequest)))
	response := invoke(t, server, noRead,
		readRequest("pgsage://databases/orders/briefings/latest"))
	require.Equal(t, codeScopeRequired, response.Error.Code)
	require.Contains(t, response.Error.Message, "read scope required")
	require.Empty(t, fake.asked, "a refused read reached the backend")
	noScopes := briefingAgent(nil)
	response = invoke(t, server, noScopes, readRequest("pgsage://briefings/latest"))
	require.Equal(t, codeScopeRequired, response.Error.Code)
	for name, ctx := range map[string]context.Context{
		"agent with read": briefingAgent([]Scope{ScopeRead}),
		"viewer":          WithPrincipal(context.Background(), viewerPrincipal),
		"unbound":         context.Background(),
	} {
		response = invoke(t, server, ctx, readRequest("pgsage://briefings/latest"))
		require.Empty(t, response.Error.Code, "%s: %s", name, response.Error.Message)
	}
}

// Happy path: the read returns the stored briefing with content_json unchanged.
func TestResourcesReadRoundTripsTheBriefing(t *testing.T) {
	server, fake := briefingServer("orders", "billing")
	uri := "pgsage://databases/orders/briefings/latest"
	response := invoke(t, server, WithPrincipal(context.Background(), viewerPrincipal),
		readRequest(uri))
	require.Empty(t, response.Error.Code, response.Error.Message)
	contents, ok := objectMap(t, response.Result)["contents"].([]any)
	require.True(t, ok, "contents is not an array: %#v", response.Result)
	require.Len(t, contents, 1)
	content := objectMap(t, contents[0])
	require.Equal(t, uri, content["uri"])
	require.Equal(t, "application/json", content["mimeType"])
	text, _ := content["text"].(string)
	require.JSONEq(t, briefingRecordJSON, text)
	var record struct {
		ContentJSON json.RawMessage `json:"content_json"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &record))
	require.JSONEq(t, `{"total_open":4,"window":{"start":"2026-11-08T06:00:00Z",`+
		`"end":"2026-11-09T06:00:00Z"},"open_by_severity":{"critical":1,"warning":3}}`,
		string(record.ContentJSON))
	require.Equal(t, []string{"orders"}, fake.asked)
}

// The default URI names the only database; with two it needs a database.
func TestResourcesReadDefaultURI(t *testing.T) {
	server, fake := briefingServer("orders")
	response := invoke(t, server, context.Background(), readRequest("pgsage://briefings/latest"))
	require.Empty(t, response.Error.Code, response.Error.Message)
	require.Equal(t, []string{"orders"}, fake.asked)
	server, _ = briefingServer("orders", "billing")
	response = invoke(t, server, context.Background(), readRequest("pgsage://briefings/latest"))
	require.Equal(t, codeDatabaseRequired, response.Error.Code)
}

// Nil/empty: no briefing yet is "resource not found" with the reason.
func TestResourcesReadNoBriefingYet(t *testing.T) {
	server, _ := briefingServer("orders", "billing")
	response := invoke(t, server, context.Background(),
		readRequest("pgsage://databases/billing/briefings/latest"))
	require.Equal(t, codeResourceNotFound, response.Error.Code)
	require.Contains(t, response.Error.Message, "no briefing yet")
	require.Contains(t, response.Error.Message, "briefing.schedule says when")
	require.Contains(t, response.Error.Message, `"billing"`)
}

// Invalid input and database refusals are distinguishable.
func TestResourcesReadInvalidInput(t *testing.T) {
	server, fake := briefingServer("orders", "billing")
	ctx := context.Background()
	const read = `{"jsonrpc":"2.0","id":5,"method":"resources/read"`
	at := func(path string) string { return readRequest("pgsage://" + path) }
	cases := map[string]int{
		read + `}`:                                          codeInvalidParams,
		read + `,"params":{}}`:                              codeInvalidParams,
		read + `,"params":[1]}`:                             codeInvalidParams,
		read + `,"params":{"uri":7}}`:                       codeInvalidParams,
		at("databases/nope/briefings/latest"):               codeUnknownDatabase,
		at("databases//briefings/latest"):                   codeResourceNotFound,
		at("databases/orders/briefings/2"):                  codeResourceNotFound,
		at("databases/a/b/briefings/latest"):                codeResourceNotFound,
		at("briefings/latest?x=1"):                          codeResourceNotFound,
		readRequest("https://example.com/briefings/latest"): codeResourceNotFound,
	}
	for request, want := range cases {
		response := invoke(t, server, ctx, request)
		require.Equal(t, want, response.Error.Code, "%s: %s", request, response.Error.Message)
	}
	response := invoke(t, server, briefingAgent([]Scope{ScopeRead}, "orders"),
		readRequest("pgsage://databases/billing/briefings/latest"))
	require.Equal(t, codeNotPermitted, response.Error.Code)
	require.Empty(t, fake.asked, "a refused read reached the backend")
	long := strings.Repeat("x", 300)
	response = invoke(t, server, ctx, readRequest("pgsage://databases/"+long+
		"/briefings/latest"))
	require.Less(t, len(response.Error.Message), 200, "an unbounded name was echoed")
}

// Error propagation: an absent backend, a failing backend and cancellation.
func TestResourcesReadErrorPropagation(t *testing.T) {
	uri := "pgsage://briefings/latest"
	plain := NewServer(&recordingBackend{})
	response := invoke(t, plain, context.Background(), readRequest(uri))
	require.Equal(t, codeUnavailable, response.Error.Code)
	server, fake := briefingServer()
	fake.err = errors.New("pool closed: secret detail")
	response = invoke(t, server, context.Background(), readRequest(uri))
	require.Equal(t, codeInternal, response.Error.Code)
	require.NotContains(t, response.Error.Message, "secret detail")
	fake.err = fmt.Errorf("pool: %w", ErrIntentUnavailable)
	response = invoke(t, server, context.Background(), readRequest(uri))
	require.Equal(t, codeUnavailable, response.Error.Code)
	fake.err = context.Canceled
	response = invoke(t, server, context.Background(), readRequest(uri))
	require.Equal(t, codeCancelled, response.Error.Code)
}

// The production backend serves the resource from its Briefings dependency.
func TestProductionBackendServesBriefings(t *testing.T) {
	backend := &ProductionBackend{}
	_, err := backend.LatestBriefing(context.Background(), "orders")
	require.ErrorIs(t, err, ErrProductionDependencyUnavailable)
	fake := &briefingFake{records: map[string]string{"orders": briefingRecordJSON}}
	backend = &ProductionBackend{dependencies: ProductionDependencies{Briefings: fake}}
	raw, err := backend.LatestBriefing(WithDatabase(context.Background(), "orders"), "orders")
	require.NoError(t, err)
	require.JSONEq(t, briefingRecordJSON, string(raw))
}

// Streamable HTTP: resources/read carries the URI in Mcp-Name.
func TestHTTPModernResourcesReadNeedsMatchingName(t *testing.T) {
	server, _ := briefingServer("orders")
	ts := httpServer(t, server, &viewerPrincipal)
	uri := "pgsage://databases/orders/briefings/latest"
	body := `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{` + modernMeta +
		`,"uri":"` + uri + `"}}`
	resp, out := post(t, ts.URL, body, modernHeaders("resources/read", uri))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	result := objectMap(t, out["result"])
	require.Equal(t, "complete", result["resultType"])
	require.Len(t, result["contents"], 1)
	resp, out = post(t, ts.URL, body, modernHeaders("resources/read", "pgsage://other"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, float64(codeHeaderMismatch), errorCode(t, out))
	list := `{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{` + modernMeta + `}}`
	resp, out = post(t, ts.URL, list, modernHeaders("resources/list", ""))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, objectMap(t, out["result"])["resources"], 1)
}

// Concurrent access: parallel reads on one server all succeed.
func TestResourcesConcurrentReads(t *testing.T) {
	server, fake := briefingServer("orders")
	var wg sync.WaitGroup
	failures := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw := server.Handle(context.Background(),
				json.RawMessage(readRequest("pgsage://briefings/latest")))
			if !strings.Contains(string(raw), `"contents"`) {
				failures <- string(raw)
			}
		}()
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Fatalf("concurrent read failed: %s", failure)
	}
	require.Len(t, fake.asked, 16)
}

// Error propagation: an unexpected backend failure is logged with its
// database and cause, so the operator can act on it, while the caller only
// sees "internal error".
func TestResourcesReadLogsAnUnexpectedFailure(t *testing.T) {
	var logged strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	server, fake := briefingServer("orders")
	fake.err = errors.New("relation sage.briefings does not exist")
	response := invoke(t, server, context.Background(),
		readRequest("pgsage://databases/orders/briefings/latest"))
	require.Equal(t, codeInternal, response.Error.Code)
	require.NotContains(t, response.Error.Message, "sage.briefings")
	require.Contains(t, logged.String(), "database=orders")
	require.Contains(t, logged.String(), "relation sage.briefings does not exist")
	logged.Reset()
	fake.err = nil
	response = invoke(t, server, context.Background(),
		readRequest("pgsage://databases/orders/briefings/latest"))
	require.Empty(t, response.Error.Code, response.Error.Message)
	require.Empty(t, logged.String(), "a successful read was logged as a failure")
}
