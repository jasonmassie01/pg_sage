package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// dashboard-02: the pending badge must never show a partial count as exact.
// A fleet instance without a pool is reported in errors, not skipped.
// No concurrent access tests: the handler reads a snapshot of the fleet per
// request and keeps no state between requests.

type pendingCountResponse struct {
	Count  *int                `json:"count"`
	Errors []map[string]string `json:"errors"`
}

func pendingCountFleet(names ...string) *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	for _, name := range names {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name:   name,
			Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: false},
		})
	}
	return mgr
}

func decodePendingCount(t *testing.T, body []byte) pendingCountResponse {
	t.Helper()
	var resp pendingCountResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if resp.Count == nil {
		t.Fatalf("response %q has no count", body)
	}
	if resp.Errors == nil {
		t.Fatalf("response %q has no errors list", body)
	}
	return resp
}

func TestFleetPendingCount_NilPoolReportedNotConnected(t *testing.T) {
	handler := fleetPendingCountHandler(pendingCountFleet("db2", "db1"))
	w := doRequest(handler, "GET", "/api/v1/actions/pending/count", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200: %s", w.Code, w.Body)
	}
	resp := decodePendingCount(t, w.Body.Bytes())
	if *resp.Count != 0 {
		t.Errorf("count: got %d, want 0", *resp.Count)
	}
	if len(resp.Errors) != 2 {
		t.Fatalf("errors: got %v, want one per unconnected database", resp.Errors)
	}
	for i, want := range []string{"db1", "db2"} {
		got := resp.Errors[i]
		if got["database"] != want || got["error"] != "not connected" {
			t.Errorf("errors[%d]: got %v, want database=%s error=not connected",
				i, got, want)
		}
	}
}

func TestFleetPendingCount_ScopedNilPoolReportsOnlyThatDatabase(t *testing.T) {
	handler := fleetPendingCountHandler(pendingCountFleet("db1", "db2"))
	w := doRequest(handler, "GET",
		"/api/v1/actions/pending/count?database=db2", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200: %s", w.Code, w.Body)
	}
	resp := decodePendingCount(t, w.Body.Bytes())
	if len(resp.Errors) != 1 || resp.Errors[0]["database"] != "db2" {
		t.Errorf("errors: got %v, want only db2", resp.Errors)
	}
}

func TestFleetPendingCount_AllScopeReportsEveryDatabase(t *testing.T) {
	handler := fleetPendingCountHandler(pendingCountFleet("db1", "db2"))
	w := doRequest(handler, "GET",
		"/api/v1/actions/pending/count?database=all", "")
	resp := decodePendingCount(t, w.Body.Bytes())
	if len(resp.Errors) != 2 {
		t.Errorf("errors: got %v, want db1 and db2", resp.Errors)
	}
}

func TestFleetPendingCount_EmptyFleetIsExactZero(t *testing.T) {
	handler := fleetPendingCountHandler(pendingCountFleet())
	w := doRequest(handler, "GET", "/api/v1/actions/pending/count", "")
	resp := decodePendingCount(t, w.Body.Bytes())
	if *resp.Count != 0 || len(resp.Errors) != 0 {
		t.Errorf("got count=%d errors=%v, want an exact 0", *resp.Count, resp.Errors)
	}
}

func TestFleetPendingCount_RejectsBadDatabaseParam(t *testing.T) {
	handler := fleetPendingCountHandler(pendingCountFleet("db1"))
	cases := map[string]int{
		"/api/v1/actions/pending/count?database=nope":          http.StatusNotFound,
		"/api/v1/actions/pending/count?database=x%27%3B--drop": http.StatusBadRequest,
	}
	for url, want := range cases {
		w := doRequest(handler, "GET", url, "")
		if w.Code != want {
			t.Errorf("%s: status got %d, want %d", url, w.Code, want)
		}
	}
}

// Integration: a connected database counts its pending rows while an
// unconnected one is named in errors, so the badge can show "N+".
func TestPhase2_FleetPendingCount_RealDBWithNilPool(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	var findingID int
	err := pool.QueryRow(ctx,
		`INSERT INTO sage.findings
		 (category, severity, title, detail, status, object_identifier)
		 VALUES ('pending_count_partial', 'warning', 'Partial count',
		  '{}', 'open', 'pending_count_partial_obj')
		 RETURNING id`).Scan(&findingID)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO sage.action_queue
			 (finding_id, proposed_sql, action_risk, status)
			 VALUES ($1, 'SELECT 1', 'SAFE', 'pending')`, findingID); err != nil {
			t.Fatalf("insert action %d: %v", i, err)
		}
	}
	mgr := phase2MgrWithPool(pool)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "offline",
		Config: config.DatabaseConfig{Name: "offline"},
		Status: &fleet.InstanceStatus{Connected: false},
	})
	w := doRequest(fleetPendingCountHandler(mgr), "GET",
		"/api/v1/actions/pending/count", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d: %s", w.Code, w.Body)
	}
	resp := decodePendingCount(t, w.Body.Bytes())
	if *resp.Count != 2 {
		t.Errorf("count: got %d, want 2", *resp.Count)
	}
	if len(resp.Errors) != 1 || resp.Errors[0]["database"] != "offline" ||
		resp.Errors[0]["error"] != "not connected" {
		t.Errorf("errors: got %v, want only offline: not connected", resp.Errors)
	}
}
