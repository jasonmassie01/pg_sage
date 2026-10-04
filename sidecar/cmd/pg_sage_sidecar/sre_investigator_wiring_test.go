package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Roadmap 2.1 wiring: sre.llm.mode "investigator" (the default) gives
// every database's coordinator the tool-calling investigator with the
// safe EXPLAIN of its monitored database; "review" keeps the M3 single
// review turn. MCP serves the transcript through the fleet.

func TestSREInvestigatorConfig_FollowsTheMode(t *testing.T) {
	settings := config.DefaultConfig().SRE
	got := sreInvestigatorConfig(settings, nil)
	if got == nil || got.Explainer != nil {
		t.Fatalf("default mode without a monitored pool = %+v, want the investigator "+
			"without EXPLAIN", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("wired investigator config invalid: %v", err)
	}
	settings.LLM.Mode = config.SRELLMModeReview
	if got := sreInvestigatorConfig(settings, nil); got != nil {
		t.Fatalf("review mode = %+v, want no investigator", got)
	}
}

// investigatorLLM is a fake provider that ends every investigation with
// an agreeing conclusion.
func investigatorLLM(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{
			"finish_reason": "stop", "message": map[string]any{"role": "assistant",
				"content": "", "tool_calls": []map[string]any{{"id": "c", "type": "function",
					"function": map[string]any{"name": sre.ToolSubmit,
						"arguments": `{"outcome":"agree","claims":[]}`}}}}}},
			"usage": map[string]int{"total_tokens": 150}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func investigatorInstance(t *testing.T, mgr *fleet.DatabaseManager, name string,
	settings config.SREConfig) sre.Investigation {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool := openComposedPool(t, ctx, dsn)
	svc, err := newSREInvestigator(sreInvestigatorDeps{control: pool, monitored: pool,
		runner: probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1)),
		name:   name, runtimeKey: fmt.Sprintf("inv-wiring:%s:%d", name, time.Now().UnixNano()),
		settings: settings, logFn: func(string, string, ...any) {},
		llm: testLLMClient(investigatorLLM(t), true), dailyTokens: 500000,
		notices: &sre.OnceLog{}})
	if err != nil {
		t.Fatalf("investigator: %v", err)
	}
	inv, _, err := svc.Coordinator().Start(ctx, sre.Trigger{CaseID: "case:" + name,
		Kind: sre.TriggerLock, Subject: "incident " + name, IdempotencyKey: "inv:" + name})
	if err != nil || svc.Coordinator().Investigate(ctx, inv.ID) != nil {
		t.Fatalf("investigation: %v", err)
	}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
		Status: &fleet.InstanceStatus{}, Investigations: svc})
	got, err := svc.Detail(ctx, inv.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	return got.Investigation
}

func TestNewSREInvestigator_DefaultModeRunsTheToolLoop(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	inv := investigatorInstance(t, mgr, "inv_default", config.DefaultConfig().SRE)
	if inv.Summary.Investigator == nil || inv.Summary.Investigator.Stop != "final" ||
		inv.ModelTurns != 0 {
		t.Fatalf("default mode summary = %+v turns %d", inv.Summary.Investigator,
			inv.ModelTurns)
	}
	review := config.DefaultConfig().SRE
	review.LLM.Mode = config.SRELLMModeReview
	other := investigatorInstance(t, mgr, "inv_review", review)
	if other.Summary.Investigator != nil {
		t.Fatalf("review mode stored an investigator transcript: %+v",
			other.Summary.Investigator)
	}
}

func TestMCPSREAccess_GetTranscript(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	access := &fleetMCPAccess{manager: mgr}
	inv := investigatorInstance(t, mgr, "inv_mcp", config.DefaultConfig().SRE)
	res, err := access.GetTranscript(context.Background(), mcp.InvestigationRequest{
		Database: "inv_mcp", InvestigationID: string(inv.ID)}, false)
	if err != nil {
		t.Fatalf("GetTranscript: %v", err)
	}
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), sre.TranscriptSchema) ||
		!strings.Contains(string(raw), `"identifiers_kept":false`) {
		t.Fatalf("transcript = %s", raw)
	}
	if _, err := access.GetTranscript(context.Background(), mcp.InvestigationRequest{
		Database: "inv_mcp", InvestigationID: string(sre.NewUUID())}, false); !errors.Is(err,
		sre.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}
