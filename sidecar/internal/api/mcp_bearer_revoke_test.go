package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// env-race-b: a token revoked while one of its requests is in flight. The
// request that authenticated before the revocation finishes; every later
// request is refused with one 401 (a single WriteHeader) and never reaches
// the backend.

// gatedBackend holds its first list_facts call until release closes.
type gatedBackend struct {
	*bearerBackend
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *gatedBackend) ListFacts(ctx context.Context, req mcp.FactRequest) (any, error) {
	first := false
	b.once.Do(func() { first = true })
	if first {
		close(b.entered)
		<-b.release
	}
	return b.bearerBackend.ListFacts(ctx, req)
}

// headerCounter counts WriteHeader calls on top of a recorder.
type headerCounter struct {
	*httptest.ResponseRecorder
	headers int
}

func (h *headerCounter) WriteHeader(code int) {
	h.headers++
	h.ResponseRecorder.WriteHeader(code)
}

func newGatedBearerFixture(t *testing.T) (*bearerFixture, *gatedBackend) {
	t.Helper()
	pool := surfacePool(t)
	t.Setenv("PG_SAGE_LIVE_PROVISIONING", "0")
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	gated := &gatedBackend{bearerBackend: &bearerBackend{},
		entered: make(chan struct{}), release: make(chan struct{})}
	server := mcp.NewServer(gated).WithDirectory(bearerDirectory{})
	runtime, err := mcp.NewRuntime(cfg.MCP, server, nil, nil)
	require.NoError(t, err)
	h := newTestRouter(nil, cfg, pool, nil, nil, nil,
		&RuntimeDeps{MCPHandler: runtime.HTTPHandler()}, SessionAuthMiddleware(pool))
	return &bearerFixture{pool: pool, store: mcptoken.NewStore(pool),
		backend: gated.bearerBackend, h: h}, gated
}

func (f *bearerFixture) sendCounted(secret, body string) *headerCounter {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	w := &headerCounter{ResponseRecorder: httptest.NewRecorder()}
	f.h.ServeHTTP(w, req)
	return w
}

func TestMCPBearerTokenRevokedMidRequestIsRefusedOnce(t *testing.T) {
	f, gated := newGatedBearerFixture(t)
	tok := f.agentToken(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call",` +
		`"params":{"name":"list_facts","arguments":{"database":"orders"}}}`
	inFlight := make(chan *headerCounter, 1)
	go func() { inFlight <- f.sendCounted(tok.Secret, body) }()
	select {
	case <-gated.entered:
	case <-time.After(10 * time.Second):
		close(gated.release)
		t.Fatal("the first request never reached the backend")
	}
	_, err := f.store.Revoke(context.Background(), tok.ID, "admin@example.com",
		mcptoken.Audit{})
	require.NoError(t, err)

	const n = 6
	refused := make([]*headerCounter, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refused[i] = f.sendCounted(tok.Secret, body)
		}()
	}
	wg.Wait()
	close(gated.release)
	first := <-inFlight

	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	for i, w := range refused {
		require.Equal(t, http.StatusUnauthorized, w.Code, "request %d: %s", i, w.Body.String())
		require.Equal(t, 1, w.headers, "request %d wrote %d status lines", i, w.headers)
		require.Contains(t, w.Body.String(), "revoked", "request %d", i)
		require.NotContains(t, w.Body.String(), "pgs_mcp_", "request %d", i)
	}
	calls := f.backend.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, "mcp:token:"+tok.ID, calls[0].actor)
}
