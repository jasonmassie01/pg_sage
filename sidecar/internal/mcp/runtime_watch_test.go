package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// mcp-09 state transition: Ask Sage switched off while a client is
// connected hides ask_sage and the watcher tells the client once, on
// stdio and on the Streamable HTTP subscription stream.

// stdioToolNames asks for tools/list on the stdio peer.
func stdioToolNames(t *testing.T, peer *stdioPeer, id string) map[string]bool {
	t.Helper()
	peer.send(`{"jsonrpc":"2.0","id":"` + id + `","method":"tools/list"}`)
	reply := peer.next()
	require.Equal(t, id, reply["id"])
	names := map[string]bool{}
	for _, item := range objectMap(t, reply["result"])["tools"].([]any) {
		names[objectMap(t, item)["name"].(string)] = true
	}
	return names
}

func TestStdioAskSwitchedOffIsHiddenAndNotifiedOnce(t *testing.T) {
	backend := newAvailabilityBackend()
	peer := startStdio(t, NewServer(backend).WithDirectory(fleetOf("orders")))
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c"}}}`)
	require.Equal(t, float64(1), peer.next()["id"])
	peer.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	require.True(t, stdioToolNames(t, peer, "before")["ask_sage"], "Ask enabled: listed")
	peer.quiet(50 * time.Millisecond)

	backend.set(FamilyAsk, "orders", false)
	changed := peer.next()
	require.Equal(t, "notifications/tools/list_changed", changed["method"])
	peer.quiet(80 * time.Millisecond)
	after := stdioToolNames(t, peer, "after")
	require.False(t, after["ask_sage"], "Ask disabled: hidden")
	require.True(t, after["list_facts"], "ungated tools stay")
	peer.quiet(50 * time.Millisecond)
}

func TestHTTPSubscriptionNotifiesAnAvailabilityChangeOnce(t *testing.T) {
	backend := newAvailabilityBackend()
	ts := httpServer(t, NewServer(backend).WithDirectory(fleetOf("orders")), &viewerPrincipal)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body := `{"jsonrpc":"2.0","id":21,"method":"subscriptions/listen","params":{` +
		modernMeta + `,"notifications":{"toolsListChanged":true}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL,
		strings.NewReader(body))
	require.NoError(t, err)
	for k, v := range modernHeaders("subscriptions/listen", "") {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	events := sseEvents(resp.Body)
	require.Equal(t, "notifications/subscriptions/acknowledged", (<-events)["method"])

	backend.set(FamilyAsk, "orders", false)
	changed := <-events
	require.Equal(t, "notifications/tools/list_changed", changed["method"])
	meta := objectMap(t, objectMap(t, changed["params"])["_meta"])
	require.Equal(t, float64(21), meta["io.modelcontextprotocol/subscriptionId"])
	select {
	case extra, ok := <-events:
		if ok {
			t.Fatalf("a second event without a second change: %v", extra)
		}
	case <-time.After(100 * time.Millisecond):
	}
}
