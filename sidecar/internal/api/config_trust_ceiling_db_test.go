package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-01 CHECK-03: turning tier3_safe off from Settings applies live; the
// response names it as applied and reports no pending restart.
func TestGlobalPutTrustCeilingIsAppliedNotPendingRestart(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cs := store.NewConfigStore(pool)
	resetWave2GlobalConfig(t, ctx, pool)
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	base := config.DefaultConfig()
	base.Trust.Tier3Safe = true
	controller := config.NewConfigController(base, nil, apiTrustOwner{})
	put := configGlobalPutHandler(cs, controller)
	response := doRequestWithUser(put, http.MethodPut, "/api/v1/config/global",
		`{"trust.tier3_safe":"false","expected_generation":1}`, user)
	if response.Code != http.StatusOK {
		t.Fatalf("put status = %d; body=%s", response.Code, response.Body.String())
	}
	var body config.ApplyResult
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.PendingRestart) != 0 {
		t.Fatalf("pending_restart = %v, want none", body.PendingRestart)
	}
	if len(body.Applied) != 1 || body.Applied[0] != "trust.tier3_safe" {
		t.Fatalf("applied = %v, want [trust.tier3_safe]", body.Applied)
	}
	if active := controller.Active(); active.Generation != 2 || active.Config.Trust.Tier3Safe {
		t.Fatalf("active = generation %d tier3_safe %t, want 2/false",
			active.Generation, active.Config.Trust.Tier3Safe)
	}
	if !base.Trust.Tier3Safe {
		t.Fatal("the controlled path mutated the process config directly")
	}
}

// Invalid input is refused at the HTTP boundary before any owner runs.
func TestGlobalPutTrustCeilingRejectsNegativeThreshold(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	cs := store.NewConfigStore(pool)
	resetWave2GlobalConfig(t, ctx, pool)
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	base := config.DefaultConfig()
	controller := config.NewConfigController(base, nil, apiTrustOwner{})
	put := configGlobalPutHandler(cs, controller)
	response := doRequestWithUser(put, http.MethodPut, "/api/v1/config/global",
		`{"trust.rollback_threshold_pct":"-1","expected_generation":1}`, user)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("put status = %d, want 400; body=%s", response.Code, response.Body.String())
	}
	if controller.Desired().Generation != 1 {
		t.Fatal("a rejected ceiling advanced the desired generation")
	}
}
