package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// fleet-09: the fleet-wide stop latches databases added after it, and a
// per-database resume cannot release one of them.

type fleetStopSummary struct {
	EmergencyStopped      bool       `json:"emergency_stopped"`
	EmergencyStoppedCount int        `json:"emergency_stopped_count"`
	TotalDatabases        int        `json:"total_databases"`
	FleetStopped          bool       `json:"fleet_stopped"`
	FleetStoppedBy        string     `json:"fleet_stopped_by"`
	FleetStoppedAt        *time.Time `json:"fleet_stopped_at"`
}

func fleetSummary(t *testing.T, r http.Handler) fleetStopSummary {
	t.Helper()
	w := get(t, r, "/api/v1/databases")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /databases: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Fleet fleetStopSummary `json:"fleet"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Fleet
}

func addLateDatabase(mgr *fleet.DatabaseManager) {
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "late",
		Config: config.DatabaseConfig{Name: "late"},
		Status: &fleet.InstanceStatus{Connected: true, LastSeen: time.Now()},
	})
}

// CHECK-02: after stop-all and one add, the overview reports the fleet
// stop and counts every database as stopped.
func TestFleetStopOverviewCountsLaterDatabase(t *testing.T) {
	r, mgr := stopRouter(t, nil, testOperatorUser())
	if w := post(t, r, "/api/v1/emergency-stop", ""); w.Code != http.StatusOK {
		t.Fatalf("stop all: %d %s", w.Code, w.Body.String())
	}
	addLateDatabase(mgr)

	summary := fleetSummary(t, r)

	if !summary.FleetStopped || summary.FleetStoppedBy != "operator@test.com" ||
		summary.FleetStoppedAt == nil {
		t.Fatalf("summary = %+v, want fleet stopped by operator@test.com", summary)
	}
	if !summary.EmergencyStopped || summary.EmergencyStoppedCount != summary.TotalDatabases ||
		summary.TotalDatabases != 2 {
		t.Fatalf("summary = %+v, want 2 of 2 stopped", summary)
	}
}

// CHECK-03: a per-database resume during a fleet-wide stop is a 409.
func TestResumeOneDatabaseDuringFleetStopConflicts(t *testing.T) {
	r, mgr := stopRouter(t, nil, testOperatorUser())
	if w := post(t, r, "/api/v1/emergency-stop", ""); w.Code != http.StatusOK {
		t.Fatalf("stop all: %d %s", w.Code, w.Body.String())
	}

	w := post(t, r, "/api/v1/resume?database=orders", "")

	if w.Code != http.StatusConflict {
		t.Fatalf("resume orders: %d %s, want 409", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fleet-wide stop is active: resume all first") {
		t.Fatalf("body = %s, want the resume-all hint", w.Body.String())
	}
	if !mgr.InstanceStopped(mgr.GetInstance("orders")) {
		t.Fatal("orders must stay stopped")
	}
	if w := post(t, r, "/api/v1/resume", ""); w.Code != http.StatusOK {
		t.Fatalf("resume all: %d %s", w.Code, w.Body.String())
	}
	if summary := fleetSummary(t, r); summary.FleetStopped || summary.EmergencyStopped {
		t.Fatalf("summary after resume all = %+v, want running", summary)
	}
}

func TestEmergencyStopResponseListsCancelledActions(t *testing.T) {
	r, _ := stopRouter(t, nil, testOperatorUser())

	w := post(t, r, "/api/v1/emergency-stop", "")

	if w.Code != http.StatusOK {
		t.Fatalf("stop all: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Stopped   int                `json:"stopped"`
		Status    string             `json:"status"`
		Cancelled map[string][]int64 `json:"cancelled_actions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Stopped != 1 || body.Status != "stopped" || body.Cancelled == nil {
		t.Fatalf("body = %s, want stopped=1 and a cancelled_actions map", w.Body.String())
	}
}
