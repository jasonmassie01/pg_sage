package executor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

func TestApprovalReadinessBlocksModerateOutsideMaintenanceWindow(t *testing.T) {
	cfg := &config.Config{}
	cfg.Trust.Level = "autonomous"
	cfg.Trust.MaintenanceWindow = "0 2 * * *"
	exec := New(nil, cfg, nil, time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	action := store.QueuedAction{
		ActionType:  "create_index_concurrently",
		ActionRisk:  "moderate",
		Status:      "pending",
		ProposedSQL: "CREATE INDEX CONCURRENTLY idx_orders_customer ON orders(customer_id)",
		ExpiresAt:   time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	got := exec.ApprovalReadiness(action, now)

	if got.Eligible {
		t.Fatalf("Eligible = true, want false")
	}
	if got.DeferReason != "outside maintenance window" {
		t.Fatalf("DeferReason = %q", got.DeferReason)
	}
	if !got.Policy.RequiresMaintenanceWindow {
		t.Fatalf("expected maintenance window requirement")
	}
}

func TestApprovalReadinessAllowsModerateInsideMaintenanceWindow(t *testing.T) {
	cfg := &config.Config{}
	cfg.Trust.Level = "autonomous"
	cfg.Trust.MaintenanceWindow = "0 2 * * *"
	exec := New(nil, cfg, nil, time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	action := store.QueuedAction{
		ActionType:  "create_index_concurrently",
		ActionRisk:  "moderate",
		Status:      "pending",
		ProposedSQL: "CREATE INDEX CONCURRENTLY idx_orders_customer ON orders(customer_id)",
		ExpiresAt:   time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 4, 28, 2, 15, 0, 0, time.UTC)

	got := exec.ApprovalReadiness(action, now)

	if !got.Eligible {
		t.Fatalf("Eligible = false, reason %q", got.DeferReason)
	}
	if got.Policy.Decision != PolicyDecisionQueueApproval {
		t.Fatalf("Decision = %q", got.Policy.Decision)
	}
}

func TestApprovalReadinessBlocksLifecycleCooldown(t *testing.T) {
	cfg := &config.Config{}
	cfg.Trust.Level = "advisory"
	exec := New(nil, cfg, nil, time.Now().Add(-10*24*time.Hour),
		func(string, string, ...any) {})
	cooldownUntil := time.Date(2026, 4, 28, 13, 0, 0, 0, time.UTC)
	action := store.QueuedAction{
		ActionType:    "analyze_table",
		ActionRisk:    "safe",
		Status:        "pending",
		ProposedSQL:   "ANALYZE public.orders",
		ExpiresAt:     time.Date(2026, 4, 29, 0, 0, 0, 0, time.UTC),
		CooldownUntil: &cooldownUntil,
	}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	got := exec.ApprovalReadiness(action, now)

	if got.Eligible {
		t.Fatalf("Eligible = true, want false")
	}
	if got.DeferReason != "action is in cooldown" {
		t.Fatalf("DeferReason = %q", got.DeferReason)
	}
	if !got.Lifecycle.InCooldown {
		t.Fatalf("expected lifecycle cooldown flag")
	}
}
