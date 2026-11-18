package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// fleetpol-01b: the Discovered list carries each row's tags, creator,
// set and milestones, and the per-set enrollment numbers.

func TestDiscoveredToMap_CarriesSetAndMilestones(t *testing.T) {
	at := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	m := discoveredToMap(store.DiscoveredRow{ID: 1, Source: "cluster", SetName: "agent-created",
		Creator: "agent:x", Tags: map[string]string{"tier": "dev"}, CreatedAt: &at,
		EnrolledAt: &at})
	if m["set_name"] != "agent-created" || m["creator"] != "agent:x" ||
		m["tags"].(map[string]string)["tier"] != "dev" || m["enrolled_at"] != &at ||
		m["first_graded_at"] != (*time.Time)(nil) || m["baseline_at"] != (*time.Time)(nil) {
		t.Fatalf("map = %+v", m)
	}
	empty := discoveredToMap(store.DiscoveredRow{})
	if tags, ok := empty["tags"].(map[string]string); !ok || tags == nil {
		t.Fatalf("empty row tags = %#v, want an empty object, not null", empty["tags"])
	}
}

func TestDiscoveredHandlers_EnrollmentNumbersRealDB(t *testing.T) {
	deps, pool, ctx := seedDiscovered(t)
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `UPDATE sage.discovered_databases SET set_name = 'default',
		enrolled_at = $1::timestamptz, baseline_at = $1::timestamptz,
		created_at = $1::timestamptz - interval '90 seconds'
		WHERE dbname = 'a'`, now); err != nil {
		t.Fatalf("seed milestones: %v", err)
	}
	w := httptest.NewRecorder()
	listDiscoveredHandler(deps)(w, withUser(httptest.NewRequest("GET",
		"/api/v1/databases/discovered", nil), testAdminUser()))
	var out struct {
		Discovered []map[string]any `json:"discovered"`
		Enrollment []map[string]any `json:"enrollment"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil || w.Code != 200 {
		t.Fatalf("list %d: %v", w.Code, err)
	}
	if len(out.Enrollment) != 1 {
		t.Fatalf("enrollment = %+v, want one set", out.Enrollment)
	}
	e := out.Enrollment[0]
	if e["set"] != "default" || e["enrolled"] != float64(1) ||
		e["p50_seconds"] != float64(90) || e["baseline_24h_ratio"] != float64(1) {
		t.Fatalf("enrollment = %+v", e)
	}
}
