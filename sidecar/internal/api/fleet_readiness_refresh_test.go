package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// A capability refresh may run while /api/v1/fleet/readiness is served:
// every response is one whole, decodable readiness view with the hints the
// adapter provides. -race is unavailable on this machine (no cgo); this
// still catches concurrent map access panics.
func TestFleetReadinessServedDuringCapabilityRefresh(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	inst := &fleet.DatabaseInstance{Name: "azure-db", Status: &fleet.InstanceStatus{
		Connected: true, Platform: "azure",
		Capabilities: fleet.BuildProviderCapabilities("azure", false, nil)}}
	mgr.RegisterInstance(inst)
	handler := fleetReadinessHandler(mgr)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			fleet.RefreshProviderCapabilities(t.Context(), inst, "azure", nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec,
				httptest.NewRequest(http.MethodGet, "/api/v1/fleet/readiness", nil))
			var body fleet.FleetReadiness
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil ||
				rec.Code != http.StatusOK || len(body.Databases) != 1 {
				t.Errorf("response %d: code %d err %v", i, rec.Code, err)
				return
			}
			caps := body.Databases[0].Capabilities
			if caps.ExtensionHints["hypopg"] != "azure_extensions_allowlist_required" {
				t.Errorf("response %d lost the allow-list hint: %v", i, caps.ExtensionHints)
				return
			}
			if body.Databases[0].ReadyForAutoSafe || body.Summary.ReadyForAutoSafe != 0 {
				t.Errorf("response %d: ready without evidence", i)
				return
			}
		}
	}()
	wg.Wait()
}
