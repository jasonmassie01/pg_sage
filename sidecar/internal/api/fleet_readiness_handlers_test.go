package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestFleetReadinessHandlerReturnsSummaryAndDatabases(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "primary",
		Status: &fleet.InstanceStatus{
			Connected: true,
			Platform:  "postgres",
			Capabilities: fleet.ProviderCapabilities{
				Provider:         "postgres",
				ReadyForAutoSafe: true,
				ActionFamilies: []fleet.ActionFamilyReadiness{{
					ActionType: "analyze_table",
					Supported:  true,
					Decision:   "execute",
				}},
			},
		},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/readiness", nil)
	fleetReadinessHandler(mgr).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body fleet.FleetReadiness
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if body.Summary.TotalDatabases != 1 ||
		body.Summary.ReadyForAutoSafe != 1 {
		t.Fatalf("summary = %+v", body.Summary)
	}
	if body.Databases[0].Provider != "postgres" {
		t.Fatalf("provider = %q", body.Databases[0].Provider)
	}
}

func TestFleetReadinessHandlerEmptyFleet(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/readiness", nil)

	fleetReadinessHandler(nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body fleet.FleetReadiness
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if body.Summary.TotalDatabases != 0 || len(body.Databases) != 0 {
		t.Fatalf("body = %+v", body)
	}
}

func TestFleetReadinessHandlerPreservesProviderBlockers(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "replica",
		Status: &fleet.InstanceStatus{
			Connected: true,
			Platform:  "cloud-sql",
			Capabilities: fleet.ProviderCapabilities{
				Provider: "cloud-sql",
				Blockers: []string{"target is a replica"},
				ActionFamilies: []fleet.ActionFamilyReadiness{{
					ActionType:    "analyze_table",
					Supported:     false,
					Decision:      "blocked",
					BlockedReason: "target is a replica",
				}},
			},
		},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/fleet/readiness", nil)
	fleetReadinessHandler(mgr).ServeHTTP(rec, req)

	var body fleet.FleetReadiness
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if body.Summary.Blocked != 1 {
		t.Fatalf("blocked = %d, want 1", body.Summary.Blocked)
	}
	if body.Databases[0].Blockers[0] != "target is a replica" {
		t.Fatalf("blockers = %#v", body.Databases[0].Blockers)
	}
}
