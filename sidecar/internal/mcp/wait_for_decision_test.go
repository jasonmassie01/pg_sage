package mcp

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// wait_for_decision (mcp-01b): a held item's agent waits on the in-process
// live-event stream, bounded by timeout_seconds (1..60) and by waiter caps
// (5 per principal, 200 in total).

const waitArgs = `{"handle":"change_approval:7","timeout_seconds":30}`

func TestWaitForDecisionReturnsApprovedWhenAPersonApproves(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	backend.set("change_approval:7", ownedBy("a1", "pending", false))
	server := waitServer(backend, hub)
	done := startWait(server, waitAgentCtx("a1"), waitArgs)
	hub.awaitSubscribers(t, 1)
	stillWaiting(t, done, 50*time.Millisecond)
	approved := time.Now()
	backend.set("change_approval:7", RequestRecord{State: "approved",
		DecidedByKind: "person", Outcome: "approved by user 3",
		OwnerPrincipalID: agpID("a1")})
	hub.publish()
	content := mustSucceed(t, receive(t, done, time.Second))
	require.True(t, time.Since(approved) < time.Second, "the wait returned within 1 s")
	require.Equal(t, "approved", content["state"])
	require.Equal(t, "person", content["decided_by_kind"])
	require.Equal(t, "approved by user 3", content["outcome"])
	require.Equal(t, toolWaitForDecision, objectMap(t, content["next"])["tool"])
	require.Equal(t, 0, hub.count(), "the waiter unsubscribed")
}

func TestWaitForDecisionTimeoutReturnsPendingAndWaitAgain(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	backend.set("change_approval:7", ownedBy("a1", "pending", false))
	got := callTool(waitServer(backend, hub), waitAgentCtx("a1"), toolWaitForDecision,
		`{"handle":"change_approval:7","timeout_seconds":1}`)
	content := mustSucceed(t, got)
	require.True(t, got.elapsed >= 900*time.Millisecond && got.elapsed < 5*time.Second,
		"waited %s for a 1 s timeout", got.elapsed)
	require.Equal(t, "pending", content["state"])
	next := objectMap(t, content["next"])
	require.Equal(t, toolWaitForDecision, next["tool"])
	require.Equal(t, map[string]any{"handle": "change_approval:7", "database": "orders"},
		next["arguments"])
}

func TestWaitForDecisionTimeoutBoundaries(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	backend.set("plan:1", ownedBy("a1", "rejected", true))
	server := waitServer(backend, hub)
	for _, timeout := range []string{"0", "61", "-1", "1.5", `"30"`, "null", "1e9"} {
		mustFail(t, callTool(server, waitAgentCtx("a1"), toolWaitForDecision,
			`{"handle":"plan:1","timeout_seconds":`+timeout+`}`), "invalid_arguments")
	}
	for _, args := range []string{`{"handle":"plan:1","timeout_seconds":1}`,
		`{"handle":"plan:1","timeout_seconds":60}`, `{"handle":"plan:1"}`} {
		content := mustSucceed(t, callTool(server, waitAgentCtx("a1"), toolWaitForDecision,
			args))
		require.Equal(t, "rejected", content["state"], args)
	}
}

func TestWaitForDecisionOnAFinalItemReturnsAtOnce(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	backend.set("plan:2", RequestRecord{State: "verified", DecidedByKind: "person",
		Final: true, OwnerPrincipalID: agpID("a1")})
	got := callTool(waitServer(backend, hub), waitAgentCtx("a1"), toolWaitForDecision,
		`{"handle":"plan:2","timeout_seconds":60}`)
	content := mustSucceed(t, got)
	require.True(t, got.elapsed < time.Second, "a final item does not wait (%s)", got.elapsed)
	require.Equal(t, "verified", content["state"])
	require.Nil(t, content["next"])
}

func TestWaitForDecisionStreamShutdownReturnsUnknown(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	backend.set("change_approval:7", ownedBy("a1", "pending", false))
	done := startWait(waitServer(backend, hub), waitAgentCtx("a1"), waitArgs)
	hub.awaitSubscribers(t, 1)
	hub.shutdown()
	content := mustSucceed(t, receive(t, done, time.Second))
	require.Equal(t, "unknown", content["state"])
	require.Equal(t, "", content["decided_by_kind"])
	next := objectMap(t, content["next"])
	require.Equal(t, toolGetRequestStatus, next["tool"])
	require.Equal(t, map[string]any{"handle": "change_approval:7", "database": "orders"},
		next["arguments"])
	after := callTool(waitServer(backend, hub), waitAgentCtx("a1"), toolWaitForDecision,
		waitArgs)
	require.Equal(t, "unknown", mustSucceed(t, after)["state"],
		"a stream already shut down answers at once")
	require.True(t, after.elapsed < time.Second)
}

func TestWaitForDecisionStoreFailureMidWaitReturnsUnknown(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	backend.set("change_approval:7", ownedBy("a1", "pending", false))
	done := startWait(waitServer(backend, hub), waitAgentCtx("a1"), waitArgs)
	hub.awaitSubscribers(t, 1)
	backend.fail(errStoreDown)
	hub.publish()
	content := mustSucceed(t, receive(t, done, time.Second))
	require.Equal(t, "unknown", content["state"])
	require.Equal(t, toolGetRequestStatus, objectMap(t, content["next"])["tool"])
	require.NotContains(t, content["outcome"], "connection refused", "no store internals")
}

func TestWaitForDecisionWithoutAStreamIsUnavailable(t *testing.T) {
	backend := newStatusBackend()
	backend.set("change_approval:7", ownedBy("a1", "pending", false))
	detail := mustFail(t, callTool(waitServer(backend, nil), waitAgentCtx("a1"),
		toolWaitForDecision, waitArgs), "unavailable")
	require.Contains(t, detail["message"], "get_request_status")
	require.Equal(t, 0, backend.readCount(), "nothing read without a stream")
}

func TestWaitForDecisionCancelledCallReleasesItsSlot(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	backend.set("change_approval:7", ownedBy("a1", "pending", false))
	server := waitServer(backend, hub)
	ctx, cancel := context.WithCancel(waitAgentCtx("a1"))
	var dones []<-chan callResult
	for i := 0; i < MaxWaitersPerPrincipal; i++ {
		dones = append(dones, startWait(server, ctx, waitArgs))
	}
	hub.awaitSubscribers(t, MaxWaitersPerPrincipal)
	cancel()
	for _, done := range dones {
		receive(t, done, time.Second)
	}
	backend.set("change_approval:7", ownedBy("a1", "rejected", true))
	content := mustSucceed(t, callTool(server, waitAgentCtx("a1"), toolWaitForDecision,
		waitArgs))
	require.Equal(t, "rejected", content["state"], "the cancelled waiters freed their slots")
}

func TestWaitForDecisionContextDeadlineEndsTheWaitWithPending(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	backend.set("change_approval:7", ownedBy("a1", "pending", false))
	ctx, cancel := context.WithTimeout(waitAgentCtx("a1"), 1500*time.Millisecond)
	defer cancel()
	got := callTool(waitServer(backend, hub), ctx, toolWaitForDecision, waitArgs)
	content := mustSucceed(t, got)
	require.Equal(t, "pending", content["state"], "answered before the caller gave up")
	require.True(t, got.elapsed < 1500*time.Millisecond, "returned in %s", got.elapsed)
}

func TestWaitForDecisionSixthWaiterOfAPrincipalIsRateLimited(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	backend.set("change_approval:7", ownedBy("a1", "pending", false))
	backend.set("plan:8", ownedBy("b2", "rejected", true))
	server := waitServer(backend, hub)
	var dones []<-chan callResult
	for i := 0; i < MaxWaitersPerPrincipal; i++ {
		dones = append(dones, startWait(server, waitAgentCtx("a1"), waitArgs))
	}
	hub.awaitSubscribers(t, MaxWaitersPerPrincipal)
	detail := mustFail(t, callTool(server, waitAgentCtx("a1"), toolWaitForDecision,
		waitArgs), "rate_limited")
	retry, ok := detail["retry_after"].(float64)
	require.True(t, ok && retry >= 1 && retry <= 30, "retry_after %v", detail["retry_after"])
	require.Equal(t, "wait", detail["next"])
	require.Contains(t, detail["message"], "5")
	other := mustSucceed(t, callTool(server, waitAgentCtx("b2"), toolWaitForDecision,
		`{"handle":"plan:8","timeout_seconds":5}`))
	require.Equal(t, "rejected", other["state"], "another principal is not limited")
	backend.set("change_approval:7", ownedBy("a1", "approved", false))
	hub.publish()
	for _, done := range dones {
		require.Equal(t, "approved", mustSucceed(t, receive(t, done, time.Second))["state"])
	}
	backend.set("change_approval:7", ownedBy("a1", "expired", true))
	require.Equal(t, "expired", mustSucceed(t, callTool(server, waitAgentCtx("a1"),
		toolWaitForDecision, waitArgs))["state"], "slots are free again")
}

// Concurrency: 200 waiters (5 for each of 40 principals) on 200 items all
// resolve to their own item's state, the 201st is refused, and no
// goroutine outlives the waits.
func TestWaitForDecision200WaitersResolveWithoutLeaks(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	server := waitServer(backend, hub)
	baseline := runtime.NumGoroutine()
	handle := func(i int) string { return fmt.Sprintf("change_approval:%d", i+1) }
	owner := func(i int) string { return fmt.Sprintf("p%02d", i/MaxWaitersPerPrincipal) }
	for i := 0; i < MaxWaitersTotal; i++ {
		backend.set(handle(i), ownedBy(owner(i), "pending", false))
	}
	results := make([]callResult, MaxWaitersTotal)
	var wg sync.WaitGroup
	for i := 0; i < MaxWaitersTotal; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = callTool(server, waitAgentCtx(owner(i)), toolWaitForDecision,
				`{"handle":"`+handle(i)+`","timeout_seconds":30}`)
		}(i)
	}
	hub.awaitSubscribers(t, MaxWaitersTotal)
	backend.set("plan:1", ownedBy("zz", "pending", false))
	detail := mustFail(t, callTool(server, waitAgentCtx("zz"), toolWaitForDecision,
		`{"handle":"plan:1","timeout_seconds":30}`), "rate_limited")
	require.Contains(t, detail["message"], "200")
	for i := 0; i < MaxWaitersTotal; i++ {
		state := "approved"
		if i%2 == 1 {
			state = "rejected"
		}
		backend.set(handle(i), ownedBy(owner(i), state, state == "rejected"))
	}
	hub.publish()
	wg.Wait()
	for i, got := range results {
		content := mustSucceed(t, got)
		require.Equal(t, handle(i), content["handle"])
		want := map[bool]string{false: "approved", true: "rejected"}[i%2 == 1]
		require.Equal(t, want, content["state"], handle(i))
	}
	require.Equal(t, 0, hub.count(), "every waiter unsubscribed")
	requireGoroutinesBackTo(t, baseline)
}

func requireGoroutinesBackTo(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d, baseline %d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// State transitions: each wait returns the item's next real state, from
// the state it found when it started.
func TestWaitForDecisionFollowsEveryTransition(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	server := waitServer(backend, hub)
	ctx := waitAgentCtx("a1")
	steps := []RequestRecord{
		{State: "approved", DecidedByKind: "person"},
		{State: "executed", DecidedByKind: "person"},
		{State: "verified", DecidedByKind: "person", Final: true, Outcome: "improved"}}
	backend.set("change_approval:7", ownedBy("a1", "pending", false))
	for _, step := range steps {
		done := startWait(server, ctx, waitArgs)
		hub.awaitSubscribers(t, 1)
		hub.publish() // an event that changes nothing does not end the wait
		stillWaiting(t, done, 100*time.Millisecond)
		step.OwnerPrincipalID = agpID("a1")
		backend.set("change_approval:7", step)
		hub.publish()
		content := mustSucceed(t, receive(t, done, time.Second))
		require.Equal(t, step.State, content["state"])
		require.Equal(t, step.DecidedByKind, content["decided_by_kind"])
		require.Equal(t, step.Outcome, content["outcome"])
		require.Equal(t, step.Final, content["next"] == nil, step.State)
	}
	backend.set("plan:4", ownedBy("a1", "pending", false))
	done := startWait(server, ctx, `{"handle":"plan:4","timeout_seconds":30}`)
	hub.awaitSubscribers(t, 1)
	backend.set("plan:4", RequestRecord{State: "rejected", DecidedByKind: "person",
		Final: true, OwnerPrincipalID: agpID("a1")})
	hub.publish()
	content := mustSucceed(t, receive(t, done, time.Second))
	require.Equal(t, "rejected", content["state"])
	require.Nil(t, content["next"])
}

func TestWaitForDecisionLosesOwnershipMidWaitIsNotFound(t *testing.T) {
	backend, hub := newStatusBackend(), newFakeHub()
	backend.set("change_approval:7", ownedBy("a1", "pending", false))
	done := startWait(waitServer(backend, hub), waitAgentCtx("a1"), waitArgs)
	hub.awaitSubscribers(t, 1)
	backend.set("change_approval:7", ownedBy("b2", "approved", false))
	hub.publish()
	mustFail(t, receive(t, done, time.Second), "not_found")
}

// The HTTP transport extends its write deadline only for a wait.
func TestWaitCallIsOnlyAToolsCallOfWaitForDecision(t *testing.T) {
	require.True(t, waitCall([]byte(toolCall(toolWaitForDecision, waitArgs))))
	for _, body := range []string{toolCall(toolGetRequestStatus, waitArgs),
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, `not json`, ``,
		`{"method":"tools/call","params":{"name":7}}`} {
		require.False(t, waitCall([]byte(body)), body)
	}
	require.True(t, waitWriteDeadline > MaxWaitSeconds*time.Second,
		"the answer outlives the longest wait")
}
