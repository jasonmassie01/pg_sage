package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestRestartFirstFleetResponseShowsStop automates the D8 first-paint check:
// process 1 stops the database through the API; process 2 (a fresh manager
// and router over the same database, as after a sidecar restart) must report
// the stop, its attribution and blocked readiness in its first response.
func TestRestartFirstFleetResponseShowsStop(t *testing.T) {
	pool := stopTestDB(t)
	before, _ := stopRouter(t, pool, testOperatorUser())
	if w := post(t, before, "/api/v1/emergency-stop?database=orders", ""); w.Code != 200 {
		t.Fatalf("stop before restart: %d %s", w.Code, w.Body.String())
	}

	after, _ := stopRouter(t, pool, testViewerUser())
	w := get(t, after, "/api/v1/databases")

	if w.Code != http.StatusOK {
		t.Fatalf("first GET /databases: %d", w.Code)
	}
	var body struct {
		Summary struct {
			EmergencyStopped      bool `json:"emergency_stopped"`
			EmergencyStoppedCount int  `json:"emergency_stopped_count"`
		} `json:"summary"`
		Databases []struct {
			databaseStopView
			Status struct {
				Capabilities struct {
					ReadyForAutoSafe bool `json:"ready_for_auto_safe"`
				} `json:"capabilities"`
			} `json:"status"`
		} `json:"databases"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Summary.EmergencyStopped || body.Summary.EmergencyStoppedCount != 1 {
		t.Fatalf("summary = %+v, want emergency_stopped=true, count 1", body.Summary)
	}
	if len(body.Databases) != 1 {
		t.Fatalf("databases = %d, want 1", len(body.Databases))
	}
	db := body.Databases[0]
	if !db.EmergencyStopped || db.EmergencyStoppedBy != "operator@test.com" ||
		db.EmergencyStoppedAt == nil {
		t.Fatalf("database = %+v, want stopped by operator@test.com", db.databaseStopView)
	}
	if db.Status.Capabilities.ReadyForAutoSafe {
		t.Fatal("a stopped database must not report ready_for_auto_safe after restart")
	}
}
