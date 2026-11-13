package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// The MCP upgrade_readiness adapter resolves pools from the fleet and
// never dials for a request it can refuse first (the pools are lazy).
func TestUpgradeReadinessMCP_Resolution(t *testing.T) {
	ctx := context.Background()
	if _, err := (upgradeReadinessMCP{}).UpgradeReadiness(ctx,
		mcp.UpgradeReadinessRequest{TargetMajor: 18}); !errors.Is(err, upgradecheck.ErrNoPool) {
		t.Fatalf("nil manager = %v, want ErrNoPool", err)
	}
	mgr, _ := managerWith(t, "app")
	u := upgradeReadinessMCP{manager: mgr}
	onApp := mcp.WithDatabase(ctx, "app")
	_, err := u.UpgradeReadiness(onApp, mcp.UpgradeReadinessRequest{TargetMajor: 18,
		TargetDatabase: "ghost"})
	if !errors.Is(err, upgradecheck.ErrInvalidTarget) || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown target_database = %v, want ErrInvalidTarget naming it", err)
	}
	_, err = u.UpgradeReadiness(mcp.WithDatabase(ctx, "missing"),
		mcp.UpgradeReadinessRequest{TargetMajor: 18})
	if err == nil || !strings.Contains(err.Error(), `"missing" has no connection pool`) {
		t.Fatalf("unknown database = %v, want a no-pool error", err)
	}
	_, err = u.UpgradeReadiness(onApp, mcp.UpgradeReadinessRequest{TargetMajor: 19})
	if !errors.Is(err, upgradecheck.ErrInvalidTarget) {
		t.Fatalf("target 19 = %v, want ErrInvalidTarget before any connection", err)
	}
}
