package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/specialist"
)

// Router configurations that mount the optional routes the four original
// configurations leave out, so every registered route has a golden row.

type stubSpecialistAudit struct{}

func (stubSpecialistAudit) Recent(context.Context, int) ([]specialist.Record, error) {
	return nil, nil
}

// stubSpecialist answers like the real contract does without a bearer.
var stubSpecialist = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	jsonError(w, "specialist bearer token required", http.StatusUnauthorized)
})

// buildOptionalRouter is the standalone router plus pprof, the specialist
// contract and its audit, and Ask Sage.
func buildOptionalRouter(t *testing.T, mw func(http.Handler) http.Handler) http.Handler {
	cfg := config.DefaultConfig()
	cfg.MCP.Enabled, cfg.MCP.Transport = true, "http"
	cfg.Debug.PprofEnabled = true
	mgr := fleet.NewManager(cfg)
	pool := unreachablePool(t)
	mcp := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	rt := &RuntimeDeps{
		MCPHandler:      mcp,
		Autonomy:        &AutonomyDeps{Ledgers: earned.NewRegistry(false)},
		Specialist:      stubSpecialist,
		SpecialistAudit: stubSpecialistAudit{},
		Ask:             ask.NewRegistry(),
	}
	return newTestRouter(mgr, cfg, pool, nil, nil, nil, rt, mw)
}

// buildDatabricksAppRouter is a Databricks App deployment: platform
// identity, no local login, no MCP or signed-ingest transports.
func buildDatabricksAppRouter(t *testing.T, mw func(http.Handler) http.Handler) http.Handler {
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	pool := unreachablePool(t)
	rt := &RuntimeDeps{DatabricksAppAuth: true}
	return newTestRouter(mgr, cfg, pool, nil, nil, nil, rt, mw)
}
