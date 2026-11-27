package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// The stdio transport survives an oversized line, runs tools/call off the
// reader so one slow call never blocks the session, and honours
// notifications/cancelled (mcp-04).

// blockingBackend holds every get_policy call until the test releases it
// (one token per call) or the call's context ends.
type blockingBackend struct {
	recordingBackend
	started chan struct{}
	release chan struct{}
	ended   chan error
}

func newBlockingBackend() *blockingBackend {
	return &blockingBackend{started: make(chan struct{}, 16),
		release: make(chan struct{}, 16), ended: make(chan error, 16)}
}

func (b *blockingBackend) GetPolicy(ctx context.Context, _ PolicyRequest) (PolicyResult,
	error) {
	b.started <- struct{}{}
	select {
	case <-b.release:
		b.ended <- nil
		return PolicyResult{Version: 7, Profile: "staffed"}, nil
	case <-ctx.Done():
		// A release queued before the context ended wins: select picks
		// at random when both are ready, which would make a test that
		// releases and then cancels flaky.
		select {
		case <-b.release:
			b.ended <- nil
			return PolicyResult{Version: 7, Profile: "staffed"}, nil
		default:
		}
		b.ended <- ctx.Err()
		return PolicyResult{}, ctx.Err()
	}
}

func (b *blockingBackend) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("tool call never reached the backend")
	}
}

func (b *blockingBackend) waitEnded(t *testing.T) error {
	t.Helper()
	select {
	case err := <-b.ended:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("tool call never ended")
	}
	return nil
}

func callLine(id string) string {
	return `{"jsonrpc":"2.0","id":"` + id + `","method":"tools/call","params":` +
		`{"name":"get_policy","arguments":{"database_id":42}}}`
}

func cancelLine(id string) string {
	return `{"jsonrpc":"2.0","method":"notifications/cancelled","params":` +
		`{"requestId":"` + id + `","reason":"user"}}`
}

func pingLine(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"}`, id)
}

func rpcErrorOf(t *testing.T, message map[string]any) (float64, string) {
	t.Helper()
	failed := objectMap(t, message["error"])
	code, _ := failed["code"].(float64)
	text, _ := failed["message"].(string)
	return code, text
}

func (p *stdioPeer) requireServing() {
	p.t.Helper()
	select {
	case err := <-p.done:
		p.t.Fatalf("Serve returned %v; it must keep serving", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func (p *stdioPeer) requirePong(id int) {
	p.t.Helper()
	pong := p.next()
	require.Equal(p.t, float64(id), pong["id"])
	require.NotContains(p.t, pong, "error")
	require.Equal(p.t, map[string]any{}, objectMap(p.t, pong["result"]))
}

// paddedPing is a valid ping exactly size bytes long.
func paddedPing(id, size int) string {
	head := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"ping"`, id)
	return head + strings.Repeat(" ", size-len(head)-1) + "}"
}

// Invalid input: a 5 MiB line is answered with -32600 and an id of null,
// and the session keeps serving the next request.
func TestStdioOversizedLineIsRejectedAndServeContinues(t *testing.T) {
	peer := startStdio(t, NewServer(&recordingBackend{}))
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` +
		strings.Repeat("x", 5<<20) + `"}}`)
	peer.send(pingLine(2))
	rejected := peer.next()
	require.Contains(t, rejected, "id")
	require.Nil(t, rejected["id"])
	code, text := rpcErrorOf(t, rejected)
	require.Equal(t, float64(codeInvalidRequest), code)
	require.Equal(t, "message exceeds 4 MiB", text)
	peer.requirePong(2)
	peer.requireServing()
}

// Boundary: a line of exactly maxStdioLine bytes is read whole; one byte
// more is rejected, and the line after it is still served.
func TestStdioLineSizeBoundary(t *testing.T) {
	peer := startStdio(t, NewServer(&recordingBackend{}))
	peer.send(paddedPing(7, maxStdioLine))
	peer.requirePong(7)
	peer.send(paddedPing(8, maxStdioLine+1))
	code, _ := rpcErrorOf(t, peer.next())
	require.Equal(t, float64(codeInvalidRequest), code)
	peer.send(pingLine(9))
	peer.requirePong(9)
	peer.requireServing()
}

// A CRLF-terminated line is one message, as with the line scanner.
func TestStdioAcceptsCRLFLines(t *testing.T) {
	peer := startStdio(t, NewServer(&recordingBackend{}))
	peer.send(pingLine(3) + "\r")
	peer.requirePong(3)
}

// Nil/empty: blank lines are ignored, not answered with a parse error.
func TestStdioIgnoresEmptyLines(t *testing.T) {
	peer := startStdio(t, NewServer(&recordingBackend{}))
	peer.send("")
	peer.send("   \t")
	peer.send(pingLine(4))
	peer.requirePong(4)
	peer.quiet(50 * time.Millisecond)
}

// Concurrency: a tool call blocked in its backend does not delay
// tools/list or ping.
func TestStdioBlockedCallDoesNotDelayToolsList(t *testing.T) {
	backend := newBlockingBackend()
	peer := startStdio(t, NewServer(backend))
	peer.send(callLine("c1"))
	backend.waitStarted(t)
	peer.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	listed := peer.next()
	require.Equal(t, float64(2), listed["id"])
	require.NotEmpty(t, objectMap(t, listed["result"])["tools"])
	peer.send(pingLine(3))
	peer.requirePong(3)
	backend.release <- struct{}{}
	answered := peer.next()
	require.Equal(t, "c1", answered["id"])
	require.NotContains(t, answered, "error")
	require.Equal(t, float64(7), structuredVersion(t, answered))
	require.NoError(t, backend.waitEnded(t))
}

func structuredVersion(t *testing.T, message map[string]any) float64 {
	t.Helper()
	result := objectMap(t, message["result"])
	version, _ := objectMap(t, result["structuredContent"])["version"].(float64)
	return version
}

// Concurrency: at most four calls run at once; a fifth starts only when
// one of the four finishes.
func TestStdioFifthConcurrentCallWaitsForASlot(t *testing.T) {
	backend := newBlockingBackend()
	peer := startStdio(t, NewServer(backend))
	for i := 1; i <= 4; i++ {
		peer.send(callLine(fmt.Sprintf("c%d", i)))
		backend.waitStarted(t)
	}
	peer.send(callLine("c5"))
	select {
	case <-backend.started:
		t.Fatal("a fifth call started while four were in flight")
	case <-time.After(200 * time.Millisecond):
	}
	backend.release <- struct{}{}
	first := peer.next()
	require.NoError(t, backend.waitEnded(t))
	backend.waitStarted(t)
	for i := 0; i < 4; i++ {
		backend.release <- struct{}{}
	}
	seen := map[any]bool{first["id"]: true}
	for i := 0; i < 4; i++ {
		answered := peer.next()
		require.NotContains(t, answered, "error")
		seen[answered["id"]] = true
	}
	require.Equal(t, map[any]bool{"c1": true, "c2": true, "c3": true, "c4": true,
		"c5": true}, seen)
}

// State transition: notifications/cancelled ends the in-flight call (the
// backend sees ctx.Done()) and no response is written for it.
func TestStdioCancelledCallGetsNoResponse(t *testing.T) {
	backend := newBlockingBackend()
	peer := startStdio(t, NewServer(backend))
	peer.send(callLine("c1"))
	backend.waitStarted(t)
	peer.send(cancelLine("c1"))
	require.ErrorIs(t, backend.waitEnded(t), context.Canceled)
	peer.quiet(100 * time.Millisecond)
	peer.send(pingLine(2))
	peer.requirePong(2)
	peer.requireServing()
}

// State transition: cancelling an id that is not in flight (unknown,
// already answered, or malformed params) is a no-op.
func TestStdioCancelUnknownIDIsANoOp(t *testing.T) {
	backend := newBlockingBackend()
	peer := startStdio(t, NewServer(backend))
	peer.send(cancelLine("nope"))
	peer.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":"bad"}`)
	peer.send(`{"jsonrpc":"2.0","method":"notifications/cancelled"}`)
	peer.send(callLine("c1"))
	backend.waitStarted(t)
	backend.release <- struct{}{}
	answered := peer.next()
	require.Equal(t, "c1", answered["id"])
	require.NoError(t, backend.waitEnded(t))
	peer.send(cancelLine("c1"))
	peer.send(pingLine(2))
	peer.requirePong(2)
	peer.quiet(50 * time.Millisecond)
}

// State transition: cancelling a subscriptions/listen id still ends that
// subscription (no list_changed for it afterwards).
func TestStdioCancelStillEndsASubscription(t *testing.T) {
	dir := fleetOf("orders")
	peer := startStdio(t, NewServer(&recordingBackend{}).WithDirectory(dir))
	peer.send(`{"jsonrpc":"2.0","id":"sub-1","method":"subscriptions/listen","params":{` +
		modernMeta + `,"notifications":{"toolsListChanged":true}}}`)
	require.Equal(t, "notifications/subscriptions/acknowledged", peer.next()["method"])
	peer.send(cancelLine("sub-1"))
	peer.send(pingLine(2))
	peer.requirePong(2)
	dir.set(DatabaseRef{Name: "orders", ID: 1}, DatabaseRef{Name: "billing", ID: 2})
	peer.quiet(100 * time.Millisecond)
}

// Invalid input: a second call reusing an id that is still in flight is
// refused, and the first call is unaffected.
func TestStdioDuplicateInFlightIDIsRejected(t *testing.T) {
	backend := newBlockingBackend()
	peer := startStdio(t, NewServer(backend))
	peer.send(callLine("c1"))
	backend.waitStarted(t)
	peer.send(callLine("c1"))
	duplicate := peer.next()
	require.Equal(t, "c1", duplicate["id"])
	code, text := rpcErrorOf(t, duplicate)
	require.Equal(t, float64(codeInvalidRequest), code)
	require.Contains(t, text, "in flight")
	backend.release <- struct{}{}
	answered := peer.next()
	require.Equal(t, "c1", answered["id"])
	require.NotContains(t, answered, "error")
}

// Nil/empty: EOF with a call in flight waits for that call, writes its
// response, and then Serve returns nil.
func TestStdioEOFWaitsForInFlightCall(t *testing.T) {
	backend := newBlockingBackend()
	peer := startStdio(t, NewServer(backend))
	peer.send(callLine("c1"))
	backend.waitStarted(t)
	require.NoError(t, peer.in.Close())
	peer.requireServing()
	backend.release <- struct{}{}
	answered := peer.next()
	require.Equal(t, "c1", answered["id"])
	require.NotContains(t, answered, "error")
	select {
	case err := <-peer.done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the in-flight call finished")
	}
}
