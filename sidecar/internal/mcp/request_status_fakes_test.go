package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Fakes for get_request_status and wait_for_decision (mcp-01b): a status
// store keyed by database and handle, and an in-process event hub that
// stands for the live-event stream.

// statusBackend answers RequestStatus from items; err fails every read.
type statusBackend struct {
	recordingBackend
	mu    sync.Mutex
	items map[string]RequestRecord
	reads int
	err   error
}

func newStatusBackend() *statusBackend {
	return &statusBackend{items: map[string]RequestRecord{}}
}

func (b *statusBackend) RequestStatus(_ context.Context, q RequestQuery) (RequestRecord,
	error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	if b.err != nil {
		return RequestRecord{}, b.err
	}
	rec, ok := b.items[q.Database+"/"+q.Kind+":"+q.ID]
	if !ok {
		return RequestRecord{}, ErrRequestNotFound
	}
	return rec, nil
}

// set stores rec for handle on the "orders" database.
func (b *statusBackend) set(handle string, rec RequestRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items["orders/"+handle] = rec
}

func (b *statusBackend) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

func (b *statusBackend) readCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

// fakeHub is the live-event stream: publish wakes every subscriber,
// shutdown closes every channel and refuses new subscribers.
type fakeHub struct {
	mu     sync.Mutex
	subs   map[chan struct{}]bool
	closed bool
}

func newFakeHub() *fakeHub { return &fakeHub{subs: map[chan struct{}]bool{}} }

func (h *fakeHub) Subscribe() (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{}, 1)
	if h.closed {
		close(ch)
		return ch, func() {}
	}
	h.subs[ch] = true
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.subs[ch] {
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func (h *fakeHub) publish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *fakeHub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *fakeHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// awaitSubscribers waits until n waiters listen on the hub.
func (h *fakeHub) awaitSubscribers(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d waiters subscribed", h.count(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// agentPrincipalOf is an agent token acting for principal "agp_"+id.
func agentPrincipalOf(id string) Principal {
	return Principal{Actor: "token:" + id, Kind: KindAgent, TokenID: id,
		PrincipalID: agpID(id), Scopes: []Scope{ScopeRead, ScopePropose}}
}

// agpID pads id to a well-formed agent principal id.
func agpID(id string) string {
	return "agp_" + (id + strings.Repeat("a", 20))[:20]
}

func waitAgentCtx(id string) context.Context {
	return WithPrincipal(context.Background(), agentPrincipalOf(id))
}

// ownedBy is a record of principal id in state.
func ownedBy(id, state string, final bool) RequestRecord {
	return RequestRecord{State: state, Final: final, OwnerPrincipalID: agpID(id)}
}

func waitServer(backend *statusBackend, hub *fakeHub) *Server {
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	if hub != nil {
		server = server.WithDecisionEvents(hub)
	}
	return server
}

// callResult is one tools/call answer, decoded without a *testing.T so
// goroutines can make it.
type callResult struct {
	content map[string]any
	err     map[string]any
	elapsed time.Duration
	decode  error
}

func callTool(server *Server, ctx context.Context, tool, args string) callResult {
	start := time.Now()
	raw := server.Handle(ctx, json.RawMessage(toolCall(tool, args)))
	out := callResult{elapsed: time.Since(start)}
	var response struct {
		Result struct {
			IsError           bool           `json:"isError"`
			StructuredContent map[string]any `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if out.decode = json.Unmarshal(raw, &response); out.decode != nil {
		return out
	}
	if response.Error != nil {
		out.err = map[string]any{"code": float64(response.Error.Code),
			"message": response.Error.Message}
		return out
	}
	if response.Result.IsError {
		out.err, _ = response.Result.StructuredContent["error"].(map[string]any)
		return out
	}
	out.content = response.Result.StructuredContent
	return out
}

// mustSucceed fails unless the call returned content.
func mustSucceed(t *testing.T, got callResult) map[string]any {
	t.Helper()
	require.NoError(t, got.decode)
	require.Nil(t, got.err, "the call failed")
	require.NotNil(t, got.content)
	return got.content
}

// mustFail returns the error detail, failing unless reason is reason.
func mustFail(t *testing.T, got callResult, reason string) map[string]any {
	t.Helper()
	require.NoError(t, got.decode)
	require.NotNil(t, got.err, "the call succeeded: %v", got.content)
	require.Equal(t, reason, got.err["reason"], "error %v", got.err)
	return got.err
}

// startWait runs a wait in the background; its result arrives on the
// returned channel.
func startWait(server *Server, ctx context.Context, args string) <-chan callResult {
	done := make(chan callResult, 1)
	go func() { done <- callTool(server, ctx, toolWaitForDecision, args) }()
	return done
}

// receive is the wait's result within limit.
func receive(t *testing.T, done <-chan callResult, limit time.Duration) callResult {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(limit):
		t.Fatalf("the wait did not return within %s", limit)
	}
	return callResult{}
}

// stillWaiting fails if the wait returned within d.
func stillWaiting(t *testing.T, done <-chan callResult, d time.Duration) {
	t.Helper()
	select {
	case got := <-done:
		t.Fatalf("the wait returned early: %+v", got)
	case <-time.After(d):
	}
}

var errStoreDown = errors.New("read request status: dial tcp: connection refused")
