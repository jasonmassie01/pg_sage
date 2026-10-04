package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Conformance over the real stdio transport: newline-delimited JSON-RPC on
// a pipe, the legacy list_changed notification after initialize and the
// 2026-07-28 subscriptions/listen stream.

type stdioPeer struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan map[string]any
	cancel context.CancelFunc
	done   chan error
}

func startStdio(t *testing.T, server *Server) *stdioPeer {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "stdio"},
		server, inR, outW)
	require.NoError(t, err)
	runtime.SetWatchInterval(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	peer := &stdioPeer{t: t, in: inW, lines: make(chan map[string]any, 64), cancel: cancel,
		done: make(chan error, 1)}
	go func() {
		peer.done <- runtime.Serve(ctx)
		_ = outW.Close()
	}()
	go func() {
		scanner := bufio.NewScanner(outR)
		for scanner.Scan() {
			var message map[string]any
			if json.Unmarshal(scanner.Bytes(), &message) != nil {
				message = map[string]any{"unparseable": scanner.Text()}
			}
			peer.lines <- message
		}
		close(peer.lines)
	}()
	t.Cleanup(func() { cancel(); _ = inW.Close() })
	return peer
}

func (p *stdioPeer) send(line string) {
	p.t.Helper()
	_, err := p.in.Write([]byte(line + "\n"))
	require.NoError(p.t, err)
}

func (p *stdioPeer) next() map[string]any {
	p.t.Helper()
	select {
	case message, ok := <-p.lines:
		require.True(p.t, ok, "stdio stream closed")
		return message
	case <-time.After(5 * time.Second):
		p.t.Fatal("no message on the stdio stream")
	}
	return nil
}

func (p *stdioPeer) quiet(d time.Duration) {
	p.t.Helper()
	select {
	case message := <-p.lines:
		p.t.Fatalf("unexpected message: %v", message)
	case <-time.After(d):
	}
}

func TestStdioLegacySessionGetsListChanged(t *testing.T) {
	dir := fleetOf("orders")
	peer := startStdio(t, NewServer(&recordingBackend{}).WithDirectory(dir))
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c"}}}`)
	init := peer.next()
	require.Equal(t, float64(1), init["id"])
	peer.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	peer.quiet(50 * time.Millisecond)
	dir.set(DatabaseRef{Name: "orders", ID: 1}, DatabaseRef{Name: "billing", ID: 2})
	changed := peer.next()
	require.Equal(t, "notifications/tools/list_changed", changed["method"])
	require.NotContains(t, changed, "id")
	peer.quiet(50 * time.Millisecond)
}

func TestStdioDoesNotNotifyBeforeInitialize(t *testing.T) {
	dir := fleetOf("orders")
	peer := startStdio(t, NewServer(&recordingBackend{}).WithDirectory(dir))
	dir.set(DatabaseRef{Name: "billing", ID: 2})
	peer.quiet(80 * time.Millisecond)
	peer.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	require.Equal(t, float64(2), peer.next()["id"])
}

func TestStdioModernSubscription(t *testing.T) {
	dir := fleetOf("orders")
	peer := startStdio(t, NewServer(&recordingBackend{}).WithDirectory(dir))
	peer.send(`{"jsonrpc":"2.0","id":"sub-1","method":"subscriptions/listen","params":{` +
		modernMeta + `,"notifications":{"toolsListChanged":true,"promptsListChanged":true}}}`)
	ack := peer.next()
	require.Equal(t, "notifications/subscriptions/acknowledged", ack["method"])
	params := objectMap(t, ack["params"])
	require.Equal(t, "sub-1", objectMap(t, params["_meta"])["io.modelcontextprotocol/subscriptionId"])
	require.Equal(t, map[string]any{"toolsListChanged": true}, params["notifications"],
		"unsupported types are left out of the acknowledgment")

	peer.send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	require.Equal(t, float64(3), peer.next()["id"], "the stream does not block requests")

	dir.set(DatabaseRef{Name: "billing", ID: 2})
	changed := peer.next()
	require.Equal(t, "notifications/tools/list_changed", changed["method"])
	meta := objectMap(t, objectMap(t, changed["params"])["_meta"])
	require.Equal(t, "sub-1", meta["io.modelcontextprotocol/subscriptionId"])

	peer.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":` +
		`{"requestId":"sub-1"}}`)
	peer.quiet(30 * time.Millisecond)
	dir.set(DatabaseRef{Name: "orders", ID: 1})
	peer.quiet(80 * time.Millisecond)
}

func TestStdioSubscriptionClosesGracefullyOnShutdown(t *testing.T) {
	peer := startStdio(t, NewServer(&recordingBackend{}).WithDirectory(fleetOf("orders")))
	peer.send(`{"jsonrpc":"2.0","id":7,"method":"subscriptions/listen","params":{` +
		modernMeta + `,"notifications":{"toolsListChanged":true}}}`)
	require.Equal(t, "notifications/subscriptions/acknowledged", peer.next()["method"])
	peer.cancel()
	final := peer.next()
	require.Equal(t, float64(7), final["id"])
	result := objectMap(t, final["result"])
	require.Equal(t, "complete", result["resultType"])
	require.Equal(t, float64(7),
		objectMap(t, result["_meta"])["io.modelcontextprotocol/subscriptionId"])
	select {
	case err := <-peer.done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

func TestStdioSubscriptionWithoutToolsIsAcknowledgedEmpty(t *testing.T) {
	peer := startStdio(t, NewServer(&recordingBackend{}).WithDirectory(fleetOf("orders")))
	peer.send(`{"jsonrpc":"2.0","id":8,"method":"subscriptions/listen","params":{` +
		modernMeta + `,"notifications":{"resourceSubscriptions":["file:///x"]}}}`)
	ack := peer.next()
	require.Equal(t, map[string]any{}, objectMap(t, ack["params"])["notifications"])
}

func TestServerFingerprintTracksTheToolList(t *testing.T) {
	dir := fleetOf("orders")
	server := NewServer(&recordingBackend{}).WithDirectory(dir)
	first := server.Fingerprint()
	require.Equal(t, first, server.Fingerprint())
	dir.set(DatabaseRef{Name: "orders", ID: 1}, DatabaseRef{Name: "billing", ID: 2})
	require.NotEqual(t, first, server.Fingerprint())
	dir.set(DatabaseRef{Name: "orders", ID: 9})
	require.Equal(t, first, server.Fingerprint(), "ids are not part of the tool list")
}
