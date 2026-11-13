package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
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

// Each fleet member's U7 reads its own detected provider; the process's
// cloud environment is only the fallback for a member that reports none.
func TestUpgradeReadinessMCP_ProviderIsTheMembers(t *testing.T) {
	saved := cfg
	t.Cleanup(func() { cfg = saved })
	cfg = &config.Config{CloudEnvironment: "self-managed"}
	mgr, _ := managerWith(t, "app", "plain")
	mgr.GetInstance("app").UpdateStatus(func(s *fleet.InstanceStatus) {
		s.Platform = "aurora"
	})
	u := upgradeReadinessMCP{manager: mgr}
	for name, want := range map[string]string{"app": "aurora", "plain": "self-managed",
		"ghost": "self-managed"} {
		if got := u.provider(name); got != want {
			t.Fatalf("provider(%q) = %q, want %q", name, got, want)
		}
	}
	cfg = nil
	if got := u.provider("plain"); got != "" {
		t.Fatalf("no config: provider = %q, want empty", got)
	}
}
