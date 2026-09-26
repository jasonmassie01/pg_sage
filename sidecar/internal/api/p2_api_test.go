package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// G9-B15: one failing database must not look like "nothing pending".
func TestFleetPendingActions_ReportsPerDatabaseErrors(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	broken, err := pgxpool.New(ctx, phase2DSN())
	if err != nil {
		t.Fatalf("broken pool: %v", err)
	}
	broken.Close()
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "good", Pool: pool,
		Status: &fleet.InstanceStatus{Connected: true}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "bad", Pool: broken,
		Status: &fleet.InstanceStatus{Connected: true}})

	for _, h := range []http.HandlerFunc{
		fleetPendingActionsHandler(mgr), fleetPendingCountHandler(mgr),
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
		var resp struct {
			Errors []map[string]string `json:"errors"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Errors) != 1 || resp.Errors[0]["database"] != "bad" {
			t.Fatalf("errors = %v, want one entry for bad; body=%s",
				resp.Errors, w.Body.String())
		}
		if resp.Errors[0]["error"] == "" {
			t.Fatalf("error entry has no message: %v", resp.Errors)
		}
	}
}

// G9-B27: the single-database pending handler must validate a
// database name instead of silently ignoring it.
func TestPendingActionsHandler_RejectsMalformedDatabase(t *testing.T) {
	h := pendingActionsHandler(nil, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET",
		"/api/v1/actions/pending?database=bad%20name!", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// SURF-11 / G9-B16: cases are ranked globally by severity, then most
// recent observation, regardless of which database or source they
// came from.
func TestSortCasesGlobally(t *testing.T) {
	now := time.Now()
	in := []cases.Case{
		{ID: "a", DatabaseName: "db1", Severity: cases.SeverityInfo,
			ObservedAt: now},
		{ID: "b", DatabaseName: "db1", Severity: cases.SeverityWarning,
			ObservedAt: now.Add(-time.Hour)},
		{ID: "c", DatabaseName: "db2", Severity: cases.SeverityCritical,
			ObservedAt: now.Add(-2 * time.Hour)},
		{ID: "d", DatabaseName: "db2", Severity: cases.SeverityWarning,
			ObservedAt: now},
		{ID: "e", DatabaseName: "db3", Severity: cases.SeverityCritical,
			ObservedAt: now},
	}
	sortCasesGlobally(in)
	want := []string{"e", "c", "d", "b", "a"}
	for i, id := range want {
		if in[i].ID != id {
			got := make([]string, len(in))
			for j := range in {
				got[j] = in[j].ID
			}
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
