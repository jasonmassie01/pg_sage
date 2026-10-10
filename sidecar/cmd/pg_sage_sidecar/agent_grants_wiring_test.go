package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/grants"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// Agent grant wiring: the routes and agent_request_capability resolve the
// grant service at call time; without a control database every call is
// unavailable (503 / -32010), and before the gate starts a request is
// refused as unavailable rather than decided without governance.

func TestAgentGrantsWithoutControlAreUnavailable(t *testing.T) {
	agentGrantMu.Lock()
	agentGrantSvc = nil
	agentGrantMu.Unlock()
	mgr, _ := managerWith(t, "app")
	c := &config.Config{}
	apiSvc := agentGrantAPI(c, mgr, nil)
	_, err := apiSvc.Grants(context.Background(), "app", grants.Filter{})
	if !errors.Is(err, agentguard.ErrUnavailable) {
		t.Fatalf("api without control = %v", err)
	}
	_, err = apiSvc.Approve(context.Background(), "app", "agp_aaaaaaaaaaaaaaaaaaaa", 1, 1)
	if !errors.Is(err, agentguard.ErrUnavailable) {
		t.Fatalf("approve without control = %v", err)
	}
	_, err = agentGrantMCP(c, mgr, nil).RequestCapability(context.Background(), "",
		mcp.CapabilityRequest{Database: "app"})
	if !errors.Is(err, agentguard.ErrUnavailable) {
		t.Fatalf("mcp without control = %v", err)
	}
	if agentGrantService(c, mgr, nil) != nil {
		t.Fatal("no control database: no grant service")
	}
}

func TestLateGrantDeciderBeforeTheGateStarts(t *testing.T) {
	agentGate.Store(nil)
	v := lateGrantDecider{}.Decide(context.Background(), decide.Request{
		PrincipalID: "agp_aaaaaaaaaaaaaaaaaaaa"})
	if v.Allowed || v.Reason != decide.ReasonUnavailable {
		t.Fatalf("before start = %+v, want unavailable", v)
	}
}

func TestAgentGrantSourcesFillTheDecider(t *testing.T) {
	mgr, _ := managerWith(t, "app")
	var cfg decide.Config
	agentGrantSources(&cfg, mgr)
	if cfg.Objects == nil || cfg.Leases == nil || cfg.Pending == nil {
		t.Fatalf("sources not set: %+v", cfg)
	}
	if _, ok := cfg.Objects.(*grants.Checker); !ok {
		t.Fatalf("objects = %T", cfg.Objects)
	}
	n, err := cfg.Pending(context.Background(), "not-a-principal")
	if n != 0 || err != nil {
		t.Fatalf("invalid principal = %d %v", n, err)
	}
	if n, err := pendingForPrincipal(context.Background(), nil,
		"agp_aaaaaaaaaaaaaaaaaaaa"); n != 0 || err != nil {
		t.Fatalf("nil fleet = %d %v", n, err)
	}
}
