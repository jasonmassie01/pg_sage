package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// The MCP read tools resolve databases through the fleet, like the REST
// API: a named database's investigator, the single database when there
// is one, and every database for a list. An id is never looked up in
// another database than the one named (CHECK-09).

func mcpSREInstance(t *testing.T, mgr *fleet.DatabaseManager, name string) sre.Investigation {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool := openComposedPool(t, ctx, dsn)
	svc, err := newSREInvestigator(sreInvestigatorDeps{control: pool, monitored: pool,
		runner: probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1)),
		name:   name, runtimeKey: fmt.Sprintf("mcp-test:%s:%d", name, time.Now().UnixNano()),
		settings: config.DefaultConfig().SRE, logFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("investigator: %v", err)
	}
	inv, _, err := svc.Coordinator().Start(ctx, sre.Trigger{CaseID: "case:" + name,
		Kind: sre.TriggerLock, Subject: "incident " + name, IdempotencyKey: "mcp:" + name})
	if err != nil || svc.Coordinator().Investigate(ctx, inv.ID) != nil {
		t.Fatalf("investigation: %v", err)
	}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
		Status: &fleet.InstanceStatus{}, Investigations: svc})
	return inv
}

func TestMCPSREAccess_ResolvesDatabasesLikeTheAPI(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	access := &fleetMCPAccess{manager: mgr}
	ctx := context.Background()
	one := mcpSREInstance(t, mgr, "mcp_one")
	res, err := access.GetInvestigation(ctx, mcp.InvestigationRequest{
		InvestigationID: string(one.ID)})
	if err != nil {
		t.Fatalf("single database, no name: %v", err)
	}
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), string(one.ID)) ||
		!strings.Contains(string(raw), `"chain_verified":true`) {
		t.Fatalf("investigation = %s", raw)
	}
	two := mcpSREInstance(t, mgr, "mcp_two")
	if _, err := access.GetInvestigation(ctx, mcp.InvestigationRequest{
		InvestigationID: string(one.ID)}); !errors.Is(err, sre.ErrInvalidRequest) {
		t.Fatalf("two databases, no name = %v, want ErrInvalidRequest", err)
	}
	if _, err := access.GetInvestigation(ctx, mcp.InvestigationRequest{
		Database: "mcp_two", InvestigationID: string(one.ID)}); !errors.Is(err,
		sre.ErrNotFound) {
		t.Fatalf("id of another database = %v, want ErrNotFound", err)
	}
	if _, err := access.GetInvestigation(ctx, mcp.InvestigationRequest{
		Database: "nope", InvestigationID: string(one.ID)}); !errors.Is(err, sre.ErrNotFound) {
		t.Fatalf("unknown database = %v, want ErrNotFound", err)
	}
	list, err := access.ListInvestigations(ctx, mcp.InvestigationRequest{})
	raw, _ = json.Marshal(list)
	if err != nil || !strings.Contains(string(raw), string(one.ID)) ||
		!strings.Contains(string(raw), string(two.ID)) {
		t.Fatalf("fleet list = %s (%v)", raw, err)
	}
	detail, _ := mgr.GetInstance("mcp_one").Investigations.Detail(ctx, one.ID)
	ev, err := access.GetEvidence(ctx, mcp.InvestigationRequest{Database: "mcp_one",
		InvestigationID: string(one.ID), EvidenceID: string(detail.Evidence[0].ID)})
	if err != nil || ev == nil {
		t.Fatalf("evidence = %v (%v)", ev, err)
	}
}
