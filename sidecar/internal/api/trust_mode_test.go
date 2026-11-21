package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// ux-06b: GET /api/v1/trust gives every row the sentence the gate's own
// inputs support (mode, mode_because), read from the database's executor;
// a database without one watches only.

func attachCeilingExecutor(t *testing.T, f *autonomyAPIFixture, name string,
	level string) *executor.Executor {
	t.Helper()
	cfg := &config.Config{}
	cfg.Trust.Level = level
	cfg.Trust.Tier3Safe, cfg.Trust.Tier3Moderate = true, true
	cfg.Trust.MaintenanceWindow = "0 2 * * *"
	exec := executor.New(surfacePool(t), cfg, time.Now().Add(-90*24*time.Hour),
		func(string, string, ...any) {})
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	entry, _ := f.reg.Lookup(name)
	// A healthy primary inside its error budget: no downgrade holds.
	entry.Limiter = entry.Service.Limiter(earned.Binding{Database: name,
		HA: primaryHA{}, Budget: okBudget{}})
	f.reg.Register(name, entry)
	exec.WithAutonomy(entry.Limiter)
	f.mgr.GetInstance(name).Executor = exec
	return exec
}

type primaryHA struct{}

func (primaryHA) HAStatus(context.Context) (earned.HAState, error) {
	return earned.HAState{Role: earned.RolePrimary}, nil
}

type okBudget struct{}

func (okBudget) ErrorBudget(context.Context, string) (earned.BudgetState, error) {
	return earned.BudgetState{Configured: true}, nil
}

func trustRowsOf(t *testing.T, body map[string]any, database string) []map[string]any {
	t.Helper()
	dbs, _ := body["databases"].([]any)
	for _, raw := range dbs {
		if db := raw.(map[string]any); db["database"] == database {
			return trustRows(t, db)
		}
	}
	t.Fatalf("no %s view in %v", database, body)
	return nil
}

func TestTrustAPI_RowsCarryTheGatesMode(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	exec := attachCeilingExecutor(t, f, "orders", "autonomous")
	bound := policy.RuntimeState{ExecutorEnabled: true, ExecutionMode: policy.ExecutionAuto,
		TrustLevel: policy.TrustAutonomous, Tier3Safe: true, Tier3Moderate: true,
		RampStart: f.clock.Now().Add(-90 * 24 * time.Hour)}
	if _, err := f.ledger.SeedGrandfathered(context.Background(), "orders", bound); err != nil {
		t.Fatal(err)
	}
	h := f.router(testViewerUser())
	code, body := autonomyCall(t, h, "GET", "/api/v1/trust?database=orders", "")
	if code != 200 {
		t.Fatalf("trust = %d %v", code, body)
	}
	vac := findTrustRow(trustRowsOf(t, body, "orders"), "hygiene", "vacuum")
	want := "acts_then_tells"
	if !executor.ASTValidationAvailable() {
		want = "asks_first" // unattended changes need approval without cgo
	}
	because, _ := vac["mode_because"].([]any)
	if vac == nil || vac["level"] != "L3" || vac["mode"] != want || len(because) == 0 {
		t.Fatalf("hygiene/vacuum = %v, want mode %s with a because", vac, want)
	}
	slot := findTrustRow(trustRowsOf(t, body, "orders"), "wal_retention", "slot_drop")
	if slot != nil && slot["mode"] != "observes" {
		t.Fatalf("slot_drop (no contract) = %v, want observes", slot)
	}
	if err := exec.SetTrustLevel("observation"); err != nil {
		t.Fatal(err)
	}
	_, body = autonomyCall(t, h, "GET", "/api/v1/trust?database=orders", "")
	vac = findTrustRow(trustRowsOf(t, body, "orders"), "hygiene", "vacuum")
	because, _ = vac["mode_because"].([]any)
	if vac["mode"] != "observes" || len(because) == 0 ||
		because[0] != "trust ceiling observation" {
		t.Fatalf("after lowering trust: %v, want observes: trust ceiling observation", vac)
	}
}

func TestTrustAPI_NoExecutorWatchesOnly(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	code, body := autonomyCall(t, f.router(testViewerUser()), "GET",
		"/api/v1/trust?database=orders", "")
	if code != 200 {
		t.Fatalf("trust = %d %v", code, body)
	}
	rows := trustRowsOf(t, body, "orders")
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, row := range rows {
		because, _ := row["mode_because"].([]any)
		if row["mode"] != "observes" || len(because) == 0 ||
			!strings.Contains(because[0].(string), "no executor") {
			t.Fatalf("row without an executor = %v, want observes: no executor", row)
		}
	}
}
