package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

type metricsStatus struct {
	HealthScore   int      `json:"health_score"`
	HealthReasons []string `json:"health_reasons"`
	DatabaseName  string   `json:"database_name"`
	CollectorRun  string   `json:"collector_last_run"`
}

func metricsTestManager(t *testing.T) (*fleet.DatabaseManager, http.Handler) {
	t.Helper()
	cfg := &config.Config{
		Mode:      "fleet",
		Collector: config.CollectorConfig{IntervalSeconds: 60},
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "x",
		Config: config.DatabaseConfig{Name: "x"},
		Status: &fleet.InstanceStatus{
			Connected: true, FindingsWarning: 2,
			CollectorLastRun: time.Now(),
		},
	})
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "y",
		Config: config.DatabaseConfig{Name: "y"},
		Status: &fleet.InstanceStatus{
			Connected: true, FindingsCritical: 1,
			CollectorLastRun: time.Now().Add(-time.Hour),
		},
	})
	return mgr, NewRouter(mgr, cfg, nil, fakeAdminMiddleware)
}

func decodeInto(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, body)
	}
}

func databasesRow(t *testing.T, r http.Handler, name string) metricsStatus {
	t.Helper()
	w := get(t, r, "/api/v1/databases")
	if w.Code != http.StatusOK {
		t.Fatalf("/databases status %d", w.Code)
	}
	var body struct {
		Databases []struct {
			Name   string        `json:"name"`
			Status metricsStatus `json:"status"`
		} `json:"databases"`
	}
	decodeInto(t, w.Body.Bytes(), &body)
	for _, db := range body.Databases {
		if db.Name == name {
			return db.Status
		}
	}
	t.Fatalf("%q missing from /databases", name)
	return metricsStatus{}
}

func metricsRow(t *testing.T, r http.Handler, name string) metricsStatus {
	t.Helper()
	w := get(t, r, "/api/v1/metrics?database="+name)
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics?database=%s status %d: %s", name, w.Code, w.Body.String())
	}
	var body struct {
		Database string        `json:"database"`
		Status   metricsStatus `json:"status"`
	}
	decodeInto(t, w.Body.Bytes(), &body)
	if body.Database != name {
		t.Fatalf("database %q, want %q", body.Database, name)
	}
	return body.Status
}

// The per-database metrics read the same scored snapshot as /databases.
func TestMetricsHandler_PerDatabaseMatchesDatabases(t *testing.T) {
	_, r := metricsTestManager(t)
	cases := map[string]struct {
		score   int
		reasons []string
	}{
		"x": {90, []string{"2 warnings"}},
		"y": {25, []string{"collector stale 1h", "1 critical finding"}},
	}
	for name, want := range cases {
		got := metricsRow(t, r, name)
		fromList := databasesRow(t, r, name)
		if got.HealthScore != want.score ||
			!reflect.DeepEqual(got.HealthReasons, want.reasons) {
			t.Fatalf("%s metrics: score %d reasons %q, want %d %q",
				name, got.HealthScore, got.HealthReasons, want.score, want.reasons)
		}
		if got.HealthScore != fromList.HealthScore ||
			!reflect.DeepEqual(got.HealthReasons, fromList.HealthReasons) {
			t.Fatalf("%s: metrics %d %q vs databases %d %q", name,
				got.HealthScore, got.HealthReasons,
				fromList.HealthScore, fromList.HealthReasons)
		}
		if got.DatabaseName != name {
			t.Fatalf("%s: database_name %q", name, got.DatabaseName)
		}
	}
}

func TestMetricsHandler_PerDatabaseNotFound(t *testing.T) {
	_, r := metricsTestManager(t)
	if w := get(t, r, "/api/v1/metrics?database=missing"); w.Code != http.StatusNotFound {
		t.Fatalf("missing database: status %d, want 404", w.Code)
	}
}

func TestMetricsHandler_InvalidDatabaseParam(t *testing.T) {
	_, r := metricsTestManager(t)
	w := get(t, r, "/api/v1/metrics?database=%27%3B%20DROP%20TABLE%20x")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid database: status %d, want 400", w.Code)
	}
}

func TestMetricsHandler_FleetSummaryHealthy(t *testing.T) {
	_, r := metricsTestManager(t)
	w := get(t, r, "/api/v1/metrics")
	var body struct {
		Fleet     fleet.FleetSummary `json:"fleet"`
		Databases []struct {
			Name string `json:"name"`
		} `json:"databases"`
	}
	decodeInto(t, w.Body.Bytes(), &body)
	// y is stale (25): connected and error-free, yet not healthy.
	if body.Fleet.Healthy != 1 || body.Fleet.Degraded != 1 {
		t.Fatalf("healthy %d degraded %d, want 1 and 1",
			body.Fleet.Healthy, body.Fleet.Degraded)
	}
	if len(body.Databases) != 2 || body.Databases[0].Name != "y" {
		t.Fatalf("databases %+v, want y (worst) first of 2", body.Databases)
	}
}

// UpdateStatus writers run against the per-database handler. Before the
// fix the handler marshalled inst.Status outside statusMu (a race under
// -race) and always reported health_score 0.
func TestMetricsHandler_ConcurrentStatusWriters(t *testing.T) {
	mgr, r := metricsTestManager(t)
	inst := mgr.GetInstance("x")
	// Start from 0 warnings so every read sees 0 or 1, never the
	// fixture's 2, even before the first writer runs.
	inst.UpdateStatus(func(s *fleet.InstanceStatus) { s.FindingsWarning = 0 })
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				inst.UpdateStatus(func(s *fleet.InstanceStatus) {
					s.FindingsWarning = i % 2
					s.CollectorLastRun = time.Now()
				})
			}
		}()
	}
	defer func() {
		close(stop)
		wg.Wait()
	}()
	for i := 0; i < 100; i++ {
		got := metricsRow(t, r, "x")
		if got.HealthScore != 100 && got.HealthScore != 95 {
			t.Fatalf("score %d, want 100 or 95 for 0-1 warnings", got.HealthScore)
		}
	}
}
