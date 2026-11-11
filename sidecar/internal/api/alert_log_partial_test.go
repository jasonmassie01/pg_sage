package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// alerting-09: one unreadable database must not hide the alert log of
// the databases that answered. The handler returns 200 with the rows it
// could read and names the failures in failed_databases; it returns 503
// only when every selected database failed.

type alertLogBody struct {
	Database        string           `json:"database"`
	Alerts          []map[string]any `json:"alerts"`
	FailedDatabases []string         `json:"failed_databases"`
	Error           string           `json:"error"`
}

func alertLogFleet(pools map[string]*pgxpool.Pool) *fleet.DatabaseManager {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	for name, pool := range pools {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, Pool: pool,
			Config: config.DatabaseConfig{Name: name},
		})
	}
	return mgr
}

func getAlertLog(
	t *testing.T, mgr *fleet.DatabaseManager, query string,
) (int, alertLogBody) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/alert-log"+query, nil)
	w := httptest.NewRecorder()
	alertLogHandler(mgr).ServeHTTP(w, req)
	var body alertLogBody
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body (status %d): %v", w.Code, err)
	}
	return w.Code, body
}

func TestAlertLog_PartialFleetReturnsRowsThatLoaded(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	_, err := pool.Exec(ctx, `INSERT INTO sage.alert_log
		 (severity, channel, dedup_key, status, error_message)
		 VALUES ('critical', 'slack', 'k-err', 'error', 'slack: 404 Not Found')`)
	if err != nil {
		t.Fatalf("insert alert: %v", err)
	}
	mgr := alertLogFleet(map[string]*pgxpool.Pool{
		"a": pool, "b": closedPool(t),
	})

	code, body := getAlertLog(t, mgr, "")
	if code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (error %q)", code, body.Error)
	}
	if len(body.Alerts) != 1 {
		t.Fatalf("alerts: got %d, want 1: %v", len(body.Alerts), body.Alerts)
	}
	row := body.Alerts[0]
	if row["database_name"] != "a" || row["status"] != "error" {
		t.Errorf("row: got db %v status %v, want a/error",
			row["database_name"], row["status"])
	}
	if row["error_message"] != "slack: 404 Not Found" {
		t.Errorf("error_message: got %v", row["error_message"])
	}
	if len(body.FailedDatabases) != 1 || body.FailedDatabases[0] != "b" {
		t.Errorf("failed_databases: got %v, want [b]", body.FailedDatabases)
	}
	if body.Database != "all" {
		t.Errorf("database: got %q, want all", body.Database)
	}
}

func TestAlertLog_HealthyFleetReportsNoFailures(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	mgr := alertLogFleet(map[string]*pgxpool.Pool{"a": pool})

	code, body := getAlertLog(t, mgr, "?database=a")
	if code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", code)
	}
	if body.FailedDatabases == nil || len(body.FailedDatabases) != 0 {
		t.Errorf("failed_databases: got %#v, want []", body.FailedDatabases)
	}
	if body.Alerts == nil || len(body.Alerts) != 0 {
		t.Errorf("alerts: got %#v, want []", body.Alerts)
	}
}

func TestAlertLog_EveryPoolFailingIs503(t *testing.T) {
	mgr := alertLogFleet(map[string]*pgxpool.Pool{
		"a": closedPool(t), "b": closedPool(t),
	})
	code, body := getAlertLog(t, mgr, "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", code)
	}
	if body.Error == "" {
		t.Error("503 body has no error message")
	}
	if len(body.FailedDatabases) != 2 ||
		body.FailedDatabases[0] != "a" || body.FailedDatabases[1] != "b" {
		t.Errorf("failed_databases: got %v, want [a b]", body.FailedDatabases)
	}
}

func TestAlertLog_SingleSelectedPoolFailingIs503(t *testing.T) {
	mgr := alertLogFleet(map[string]*pgxpool.Pool{
		"a": closedPool(t), "b": closedPool(t),
	})
	code, body := getAlertLog(t, mgr, "?database=b")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", code)
	}
	if len(body.FailedDatabases) != 1 || body.FailedDatabases[0] != "b" {
		t.Errorf("failed_databases: got %v, want [b]", body.FailedDatabases)
	}
}

func TestAlertLog_ZeroPoolsIsEmpty200(t *testing.T) {
	// A registered database without a pool selects no pool at all.
	mgr := alertLogFleet(map[string]*pgxpool.Pool{"offline": nil})
	code, body := getAlertLog(t, mgr, "")
	if code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", code)
	}
	if body.Alerts == nil || len(body.Alerts) != 0 {
		t.Errorf("alerts: got %#v, want []", body.Alerts)
	}
	if len(body.FailedDatabases) != 0 {
		t.Errorf("failed_databases: got %v, want none", body.FailedDatabases)
	}
}

func TestAlertLog_UnknownDatabaseIs404(t *testing.T) {
	mgr := alertLogFleet(map[string]*pgxpool.Pool{"a": closedPool(t)})
	req := httptest.NewRequest("GET", "/api/v1/alert-log?database=nope", nil)
	w := httptest.NewRecorder()
	alertLogHandler(mgr).ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", w.Code)
	}
}

func TestAlertLog_ConcurrentPartialReadsAgree(t *testing.T) {
	mgr := alertLogFleet(map[string]*pgxpool.Pool{
		"a": closedPool(t), "b": closedPool(t),
	})
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/api/v1/alert-log", nil)
			w := httptest.NewRecorder()
			alertLogHandler(mgr).ServeHTTP(w, req)
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusServiceUnavailable {
			t.Errorf("request %d: got %d, want 503", i, c)
		}
	}
}
