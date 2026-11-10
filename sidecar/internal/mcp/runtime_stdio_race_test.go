package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Races between a call's outcome and its deadline or cancel on the stdio
// transport (mcp-04). -race is unavailable on this machine (no cgo); these
// tests pin the outcomes and catch deadlocks.

// lateSuccessBackend ignores its deadline: it waits for the call's context
// to end, then reports success anyway, as a backend that finished the work
// just after the deadline would.
type lateSuccessBackend struct {
	recordingBackend
}

func (*lateSuccessBackend) GetPolicy(ctx context.Context, _ PolicyRequest) (PolicyResult,
	error) {
	<-ctx.Done()
	return PolicyResult{Version: 9, Profile: "staffed"}, nil
}

// State transition: a call whose backend succeeded after the deadline is
// answered with that result, never -32800: telling the client "timed out"
// after the work happened would invite a duplicate retry.
func TestStdioSuccessPastDeadlineIsStillAnswered(t *testing.T) {
	peer := startStdioWith(t, NewServer(&lateSuccessBackend{}), func(r *Runtime) {
		r.SetCallTimeout(50 * time.Millisecond)
	})
	peer.send(callLine("late"))
	answered := peer.next()
	require.Equal(t, "late", answered["id"])
	require.NotContains(t, answered, "error")
	require.Equal(t, float64(9), structuredVersion(t, answered))
	peer.requireServing()
}

// Concurrency: a cancel racing a call's completion either suppresses the
// answer or delivers the success, never an error, never a deadlock, and it
// never leaves a slot behind (four more calls still start afterwards).
func TestStdioCancelRacingCompletion(t *testing.T) {
	backend := newBlockingBackend()
	peer := startStdio(t, NewServer(backend))
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("r%d", i)
		peer.send(callLine(id))
		backend.waitStarted(t)
		backend.release <- struct{}{}
		peer.send(cancelLine(id))
		require.NoError(t, backend.waitEnded(t))
		peer.send(pingLine(i))
		for {
			message := peer.next()
			if raced, ok := message["id"].(string); ok && strings.HasPrefix(raced, "r") {
				require.NotContains(t, message, "error", fmt.Sprintf("round %d", i))
				continue
			}
			require.Equal(t, float64(i), message["id"])
			require.NotContains(t, message, "error")
			break
		}
	}
	for i := 1; i <= maxStdioInflight; i++ {
		peer.send(callLine(fmt.Sprintf("after-%d", i)))
		backend.waitStarted(t)
	}
}
