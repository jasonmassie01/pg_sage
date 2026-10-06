package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

type fakeFleetLearning struct {
	looks  any
	err    error
	status any
	asked  string
}

func (f *fakeFleetLearning) LookAlikes(_ context.Context, database string) (any, error) {
	f.asked = database
	return f.looks, f.err
}

func (f *fakeFleetLearning) LeaderStatus() any { return f.status }

func fleetLearnMgr(t *testing.T, pools map[string]*pgxpool.Pool) *fleet.DatabaseManager {
	t.Helper()
	cfg := &config.Config{Mode: "fleet"}
	mgr := fleet.NewManager(cfg)
	for name, pool := range pools {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
			Status: &fleet.InstanceStatus{Connected: true}})
	}
	return mgr
}

func seedOpenFinding(t *testing.T, pool *pgxpool.Pool, object string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.findings (category,
		severity, object_type, object_identifier, title, detail, status)
		VALUES ('missing_index','warning','table',$1,'Missing index','{}','open')`, object)
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
}

func getFleetFindings(t *testing.T, h http.Handler, url string, code int) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != code {
		t.Fatalf("%s status = %d body=%s, want %d", url, rec.Code, rec.Body.String(), code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return body
}

func TestFleetFindingsHandlerGroupsAcrossDatabases(t *testing.T) {
	pools := valueFleetPools(t, "fl_a", "fl_b", "fl_c")
	for _, name := range []string{"fl_a", "fl_b", "fl_c"} {
		seedOpenFinding(t, pools[name], "public.orders|btree(status)")
	}
	seedOpenFinding(t, pools["fl_a"], "public.lonely|btree(x)")
	cfg := config.DefaultConfig()
	h := fleetFindingsHandler(fleetLearnMgr(t, pools), cfg)

	body := getFleetFindings(t, h, "/api/v1/fleet/findings", http.StatusOK)
	findings, _ := body["findings"].([]any)
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want the one recurring on 3 databases", body)
	}
	f := findings[0].(map[string]any)
	if f["databases"] != float64(3) || f["object_identifier"] != "public.orders|btree(status)" {
		t.Fatalf("fleet finding = %v", f)
	}
	occ, _ := f["occurrences"].([]any)
	if len(occ) != 3 || occ[0].(map[string]any)["database"] != "fl_a" {
		t.Fatalf("drill-down = %v", occ)
	}
	if body["databases_scanned"] != float64(3) || body["min_databases"] != float64(3) {
		t.Fatalf("summary = %v", body)
	}
	two := getFleetFindings(t, h, "/api/v1/fleet/findings?min_databases=2", http.StatusOK)
	if got, _ := two["findings"].([]any); len(got) != 1 {
		t.Fatalf("min 2 = %v", two)
	}
	four := getFleetFindings(t, h, "/api/v1/fleet/findings?min_databases=4", http.StatusOK)
	if got, _ := four["findings"].([]any); len(got) != 0 {
		t.Fatalf("min 4 = %v, want none", four)
	}
}

func TestFleetFindingsHandlerRejectsBadMinimum(t *testing.T) {
	h := fleetFindingsHandler(fleet.NewManager(&config.Config{}), config.DefaultConfig())
	for _, q := range []string{"abc", "1", "0", "-3", "100001"} {
		body := getFleetFindings(t, h, "/api/v1/fleet/findings?min_databases="+q,
			http.StatusBadRequest)
		if !strings.Contains(body["error"].(string), "min_databases") {
			t.Fatalf("min_databases=%s error = %v", q, body)
		}
	}
}

func TestFleetFindingsHandlerNilAndEmptyFleet(t *testing.T) {
	body := getFleetFindings(t, fleetFindingsHandler(nil, nil), "/api/v1/fleet/findings",
		http.StatusOK)
	if got, ok := body["findings"].([]any); !ok || len(got) != 0 {
		t.Fatalf("nil fleet = %v, want an empty list", body)
	}
	if body["min_databases"] != float64(config.DefaultFleetFindingMinDatabases) {
		t.Fatalf("nil config must fall back to the default minimum: %v", body)
	}
}

func TestFleetLookalikesHandler(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "a",
		Status: &fleet.InstanceStatus{}})
	reader := &fakeFleetLearning{looks: []map[string]any{{"database": "b",
		"similarity": 0.9}}}
	h := fleetLookalikesHandler(mgr, reader)

	body := getFleetFindings(t, h, "/api/v1/fleet/lookalikes?database=a", http.StatusOK)
	if reader.asked != "a" {
		t.Fatalf("asked %q, want a", reader.asked)
	}
	looks, _ := body["lookalikes"].([]any)
	if len(looks) != 1 || body["database"] != "a" {
		t.Fatalf("body = %v", body)
	}
	getFleetFindings(t, h, "/api/v1/fleet/lookalikes", http.StatusBadRequest)
	getFleetFindings(t, h, "/api/v1/fleet/lookalikes?database=nope", http.StatusNotFound)
	reader.err = errors.New("control database down")
	body = getFleetFindings(t, h, "/api/v1/fleet/lookalikes?database=a",
		http.StatusInternalServerError)
	if strings.Contains(body["error"].(string), "control database down") {
		t.Fatal("internal errors must not leak to the client")
	}
	off := fleetLookalikesHandler(mgr, nil)
	getFleetFindings(t, off, "/api/v1/fleet/lookalikes?database=a",
		http.StatusServiceUnavailable)
}

func TestFleetLeaderHandler(t *testing.T) {
	body := getFleetFindings(t, fleetLeaderHandler(nil), "/api/v1/fleet/leader",
		http.StatusOK)
	if body["enabled"] != false {
		t.Fatalf("no elector = %v, want enabled false", body)
	}
	reader := &fakeFleetLearning{status: map[string]any{"enabled": true,
		"leader": true, "holder": "host-1"}}
	body = getFleetFindings(t, fleetLeaderHandler(reader), "/api/v1/fleet/leader",
		http.StatusOK)
	if body["leader"] != true || body["holder"] != "host-1" {
		t.Fatalf("status = %v", body)
	}
}
