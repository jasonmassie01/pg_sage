package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/ha"
)

// fleet-04 CHECK-22: GET /api/v1/databases (the fleet overview) shows
// each database's HA role and safe mode from its one monitor.
func TestDatabasesHandlerShowsTheHARolePerDatabase(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	replica := ha.NewFromProbe(func(context.Context) (bool, error) { return true, nil },
		func(string, string, ...any) {})
	replica.Check(context.Background())
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "standby", HA: replica,
		Status: &fleet.InstanceStatus{Connected: true}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "nomonitor",
		Status: &fleet.InstanceStatus{Connected: true}})
	rec := httptest.NewRecorder()
	databasesHandler(mgr)(rec, httptest.NewRequest(http.MethodGet, "/api/v1/databases", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Databases []struct {
			Name   string                     `json:"name"`
			Status map[string]json.RawMessage `json:"status"`
		} `json:"databases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"standby": `"replica"`, "nomonitor": `"unknown"`}
	if len(body.Databases) != len(want) {
		t.Fatalf("databases = %d, want %d: %s", len(body.Databases), len(want), rec.Body)
	}
	for _, db := range body.Databases {
		if got := string(db.Status["ha_role"]); got != want[db.Name] {
			t.Errorf("%s: ha_role = %s, want %s", db.Name, got, want[db.Name])
		}
		if got := string(db.Status["ha_safe_mode"]); got != "false" {
			t.Errorf("%s: ha_safe_mode = %q, want false", db.Name, got)
		}
	}
}
