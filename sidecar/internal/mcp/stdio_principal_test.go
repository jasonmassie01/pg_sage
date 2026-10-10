package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// mcp.stdio_principal (agent governance §6.4): the stdio client acts as the
// named agent principal; without one it stays the tokenless stdio agent.

type principalCapture struct {
	recordingBackend
	mu  sync.Mutex
	got Principal
}

func (b *principalCapture) GetPolicy(ctx context.Context, r PolicyRequest) (PolicyResult,
	error) {
	b.mu.Lock()
	b.got, _ = PrincipalFromContext(ctx)
	b.mu.Unlock()
	return b.recordingBackend.GetPolicy(ctx, r)
}

func stdioCallPrincipal(t *testing.T, bind string) Principal {
	t.Helper()
	backend := &principalCapture{}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "stdio"},
		NewServer(backend).WithDirectory(fleetOf("orders")), inR, outW)
	require.NoError(t, err)
	if bind != "" {
		runtime.BindStdioPrincipal(bind)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runtime.Serve(ctx); _ = outW.Close() }()
	_, err = inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
		`{"name":"get_policy","arguments":{}}}` + "\n"))
	require.NoError(t, err)
	reply := make(chan map[string]any, 1)
	go func() {
		scanner := bufio.NewScanner(outR)
		if scanner.Scan() {
			var m map[string]any
			_ = json.Unmarshal(scanner.Bytes(), &m)
			reply <- m
		}
	}()
	select {
	case m := <-reply:
		require.Equal(t, float64(1), m["id"])
	case <-time.After(5 * time.Second):
		t.Fatal("no reply on stdio")
	}
	_ = inW.Close()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.got
}

func TestStdioPrincipal_BoundPrincipalReachesTools(t *testing.T) {
	got := stdioCallPrincipal(t, "agp_abcdefghijklmnopqrst")
	require.Equal(t, "agp_abcdefghijklmnopqrst", got.PrincipalID)
	require.Equal(t, KindAgent, got.Kind)
	require.Equal(t, "stdio", got.Actor)
	require.False(t, got.Has(ScopeApprove))
}

func TestStdioPrincipal_UnboundStaysTokenless(t *testing.T) {
	got := stdioCallPrincipal(t, "")
	require.Equal(t, "", got.PrincipalID)
	require.Equal(t, KindAgent, got.Kind)
	require.True(t, got.Has(ScopePropose))
}
