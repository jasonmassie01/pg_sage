package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// CHECK-39 (read tools): on the real mounted router, a read-only token
// reads investigations through MCP, a request without a token is
// refused before any tool runs, and the read tools never mutate.

type sreMCPBackend struct {
	mcpAuthzBackend
	investigationReads int
}

func (b *sreMCPBackend) ListInvestigations(context.Context,
	mcp.InvestigationRequest) (any, error) {
	b.investigationReads++
	return map[string]any{"items": []any{}}, nil
}

func (b *sreMCPBackend) GetInvestigation(_ context.Context,
	r mcp.InvestigationRequest) (any, error) {
	b.investigationReads++
	return map[string]any{"id": r.InvestigationID}, nil
}

func (b *sreMCPBackend) GetEvidence(_ context.Context,
	r mcp.InvestigationRequest) (any, error) {
	b.investigationReads++
	return map[string]any{"id": r.EvidenceID}, nil
}

const sreGetBody = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
	`{"name":"sre_get_investigation","arguments":{"investigation_id":"abc"}}}`

func TestMountedMCPViewerReadsInvestigations(t *testing.T) {
	backend := &sreMCPBackend{}
	h := mcpRouterForBackend(t, backend, "viewer")
	out := postMCPCall(t, h, sreGetBody)
	require.Nil(t, out["error"])
	require.Equal(t, 1, backend.investigationReads)
	require.Zero(t, backend.proposals)
}

func TestMountedMCPInvestigationToolsNeedASession(t *testing.T) {
	backend := &sreMCPBackend{}
	h := mcpRouterForBackend(t, backend, "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", strings.NewReader(sreGetBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Zero(t, backend.investigationReads)
}

// mcpRouterForBackend mounts the real MCP runtime and server over any
// backend on the real router; requests carry the token of a person with
// role, or no credential for "".
func mcpRouterForBackend(t *testing.T, backend mcp.Backend, role string) http.Handler {
	t.Helper()
	return mcpRouterForUser(t, backend, role)
}
