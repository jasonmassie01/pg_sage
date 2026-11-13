package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// env-race: GET /config?database=<name> read inst.Config without the fleet
// manager lock while a dashboard edit or SIGHUP reload swapped it. Written
// for CI's -race leg; without the detector it still pins that every
// response carries one whole config, never a mix of two.

func TestRace_ConfigGetDatabaseConfigDuringUpdate(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", DatabaseID: 3,
		Status: &fleet.InstanceStatus{},
		Config: config.DatabaseConfig{Name: "orders", TrustLevel: "observation",
			Tags: []string{"observation"}}})
	handler := configGetHandler(mgr, nil)
	levels := []string{"observation", "advisory"}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			level := levels[i%2]
			cfg := config.DatabaseConfig{Name: "orders", TrustLevel: level,
				Tags: []string{level}}
			if !mgr.UpdateInstanceMetadata("orders", "orders", cfg, 3, level) {
				t.Errorf("update %d refused", i)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/config?database=orders", nil)
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("GET %d: status %d body %s", i, rec.Code, rec.Body.String())
				return
			}
			var body struct {
				TrustLevel string   `json:"trust_level"`
				Tags       []string `json:"tags"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Errorf("GET %d: decode: %v", i, err)
				return
			}
			if len(body.Tags) != 1 || body.Tags[0] != body.TrustLevel {
				t.Errorf("GET %d: torn config trust=%q tags=%v",
					i, body.TrustLevel, body.Tags)
				return
			}
		}
	}()
	wg.Wait()
}

// TestDatabaseConnectionFallsBackToConfigSnapshot pins the connection
// fallback used when an instance has no pool: it reads the per-database
// config through the locked snapshot.
func TestDatabaseConnectionFallsBackToConfigSnapshot(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", DatabaseID: 4,
		Status: &fleet.InstanceStatus{},
		Config: config.DatabaseConfig{Name: "orders", Host: "db.internal", Port: 5433,
			User: "sage", Password: "s3cret", Database: "orders"}})
	conn, password, ok := fleetManagedDBConnection(&DatabaseDeps{Fleet: mgr}, 4, "")
	if !ok || password != "s3cret" {
		t.Fatalf("ok=%v password ok=%v; want the config password", ok, password == "s3cret")
	}
	if !strings.Contains(conn, "db.internal:5433/orders") {
		t.Fatalf("connection string %q does not name db.internal:5433/orders", conn)
	}
}
