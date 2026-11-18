package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/pressure"
)

// GET /api/v1/databases/{db}/pressure (guard-01a): check_pressure over
// HTTP, for a signed-in role or a read-scope API token (the migration-11b
// pattern). The verdict arithmetic and the SQL are tested in
// internal/pressure; these tests cover the route's inputs, auth and error
// mapping. No concurrent access tests here beyond the checker's own: the
// handler keeps no state.

// pressureSource is a fixed source: lag, the tables it knows, an error.
type pressureSource struct {
	mu     sync.Mutex
	lag    float64
	known  map[string]bool
	err    error
	tables []string
}

func (s *pressureSource) Read(_ context.Context, table string) (pressure.Readings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tables = append(s.tables, table)
	if s.err != nil {
		return pressure.Readings{}, s.err
	}
	if table != "" && !s.known[table] {
		return pressure.Readings{}, pressure.ErrUnknownTable
	}
	now := time.Now()
	return pressure.Readings{Inputs: map[string]pressure.Reading{
		policy.PressureReplicaLag:   pressure.Known(s.lag, now),
		policy.PressureXIDAge:       pressure.Known(10, now),
		policy.PressureMultixactAge: pressure.Known(1, now),
		policy.PressureVacuum:       pressure.Known(0, now),
		policy.PressureLockQueue:    pressure.Known(0, now),
	}, WALPosition: pressure.Known(0, now)}, nil
}

func (s *pressureSource) WALPosition(context.Context) pressure.Reading {
	return pressure.Known(0, time.Now())
}

func pressureFleet(t *testing.T, src *pressureSource) *fleet.DatabaseManager {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	mgr := fleet.NewManager(cfg)
	for _, name := range []string{"orders", "billing"} {
		inst := &fleet.DatabaseInstance{Name: name, Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: true}}
		if src != nil {
			inst.Pressure = pressure.NewChecker(pressure.Options{Database: name, Source: src,
				WALFirstSpan: time.Millisecond})
		}
		mgr.RegisterInstance(inst)
	}
	return mgr
}

func pressureMux(mgr *fleet.DatabaseManager, tokens lintTokenValidator) *http.ServeMux {
	mux := http.NewServeMux()
	registerPressureRoutes(mux, mgr, tokens, false)
	return mux
}

func getPressure(t *testing.T, h http.Handler, path, authz string,
	user *auth.User) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if user != nil {
		req = withUser(req, user)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPressureRoute_SessionGetsTheVerdict(t *testing.T) {
	src := &pressureSource{lag: 9, known: map[string]bool{"public.events": true}}
	h := pressureMux(pressureFleet(t, src), nil)
	rec := getPressure(t, h, "/api/v1/databases/orders/pressure?table=public.events"+
		"&job=backfill-7&batch=1000", "", testViewerUser())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var body pressure.Verdict
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if body.Verdict != pressure.Slow || body.Database != "orders" ||
		body.Table != "public.events" || body.Job != "backfill-7" ||
		body.SuggestedBatch != 500 || len(body.Inputs) != len(policy.PressureInputs()) {
		t.Fatalf("body = %+v, want slow for orders/public.events with batch 500", body)
	}
	if !strings.Contains(rec.Body.String(), `"verdict":"slow"`) ||
		!strings.Contains(rec.Body.String(), `"state":"not_applicable"`) &&
			!strings.Contains(rec.Body.String(), `"state":"unknown"`) {
		t.Fatalf("wire form %s lacks the verdict or the input states", rec.Body)
	}
}

func TestPressureRoute_RefusesBadInput(t *testing.T) {
	src := &pressureSource{known: map[string]bool{"t": true}}
	h := pressureMux(pressureFleet(t, src), nil)
	cases := []struct {
		path string
		code int
	}{
		{"/api/v1/databases/nope/pressure", http.StatusNotFound},
		{"/api/v1/databases/orders/pressure?table=missing", http.StatusNotFound},
		{"/api/v1/databases/orders/pressure?table=t&batch=abc", http.StatusBadRequest},
		{"/api/v1/databases/orders/pressure?table=t&batch=-5", http.StatusBadRequest},
		{"/api/v1/databases/orders/pressure?table=a%00b", http.StatusBadRequest},
		{"/api/v1/databases/orders/pressure?table=t&job=" + strings.Repeat("j", 200),
			http.StatusBadRequest},
	}
	for _, c := range cases {
		if rec := getPressure(t, h, c.path, "", testViewerUser()); rec.Code != c.code {
			t.Errorf("%s = %d %s, want %d", c.path, rec.Code, rec.Body, c.code)
		}
	}
	if !strings.Contains(getPressure(t, h, "/api/v1/databases/orders/pressure?table=missing",
		"", testViewerUser()).Body.String(), "missing") {
		t.Errorf("a 404 for an unknown table does not name it")
	}
}

func TestPressureRoute_NoTableChecksTheDatabase(t *testing.T) {
	src := &pressureSource{}
	h := pressureMux(pressureFleet(t, src), nil)
	rec := getPressure(t, h, "/api/v1/databases/billing/pressure", "", testViewerUser())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"proceed"`) {
		t.Fatalf("database-only check = %d %s, want proceed", rec.Code, rec.Body)
	}
	if len(src.tables) != 1 || src.tables[0] != "" {
		t.Fatalf("source asked for %v, want one read with no table", src.tables)
	}
}

func TestPressureRoute_ErrorsAreActionable(t *testing.T) {
	failing := &pressureSource{err: errors.New("dial tcp 10.0.0.5:5432: connection refused")}
	rec := getPressure(t, pressureMux(pressureFleet(t, failing), nil),
		"/api/v1/databases/orders/pressure", "", testViewerUser())
	if rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "orders") {
		t.Fatalf("source failure = %d %s, want 503 naming the database", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatalf("the error leaks the connection address: %s", rec.Body)
	}
	unwired := getPressure(t, pressureMux(pressureFleet(t, nil), nil),
		"/api/v1/databases/orders/pressure", "", testViewerUser())
	if unwired.Code != http.StatusServiceUnavailable {
		t.Fatalf("no checker = %d %s, want 503", unwired.Code, unwired.Body)
	}
}

func TestPressureRoute_TokenAuth(t *testing.T) {
	src := &pressureSource{known: map[string]bool{"t": true}}
	mgr := pressureFleet(t, src)
	agent := readGrant("a1")
	agent.Kind = mcptoken.KindAgent
	noRead := readGrant("w1")
	noRead.Scopes = []string{mcptoken.ScopeApprove}
	tokens := lintTokens(map[string]mcptoken.Grant{
		lintSecret(1): readGrant("t1"), lintSecret(2): readGrant("t2", "billing"),
		lintSecret(3): agent, lintSecret(4): noRead})
	h := pressureMux(mgr, tokens)
	path := "/api/v1/databases/orders/pressure?table=t"
	cases := []struct {
		name  string
		authz string
		code  int
	}{
		{"read token", "Bearer " + lintSecret(1), http.StatusOK},
		{"agent read token", "Bearer " + lintSecret(3), http.StatusOK},
		{"token for another database", "Bearer " + lintSecret(2), http.StatusNotFound},
		{"no read scope", "Bearer " + lintSecret(4), http.StatusForbidden},
		{"unknown token", "Bearer " + lintSecret(9), http.StatusUnauthorized},
		{"no credential", "", http.StatusUnauthorized},
	}
	for _, c := range cases {
		if rec := getPressure(t, h, path, c.authz, nil); rec.Code != c.code {
			t.Errorf("%s = %d %s, want %d", c.name, rec.Code, rec.Body, c.code)
		}
	}
	if rec := getPressure(t, pressureMux(mgr, nil), path, "Bearer "+lintSecret(1),
		nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no token store = %d, want 503", rec.Code)
	}
	tokens.err = errors.New("control database down")
	if rec := getPressure(t, h, path, "Bearer "+lintSecret(1), nil); rec.Code !=
		http.StatusServiceUnavailable {
		t.Errorf("token store failure = %d, want 503", rec.Code)
	}
}

func TestPressureRoute_TokenSkipsSessionAuthOnlyOnTheRoute(t *testing.T) {
	if !skipsSessionAuth(bearerProbe(http.MethodGet, "/api/v1/databases/orders/pressure")) {
		t.Fatal("a Bearer GET to the pressure route must skip session auth")
	}
	for name, req := range map[string]*http.Request{
		"POST":        bearerProbe(http.MethodPost, "/api/v1/databases/orders/pressure"),
		"other route": bearerProbe(http.MethodGet, "/api/v1/databases/orders/review/grade"),
		"deeper path": bearerProbe(http.MethodGet, "/api/v1/databases/orders/pressure/x"),
		"traversal":   bearerProbe(http.MethodGet, "/api/v1/databases/x/../users/pressure"),
		"no database": bearerProbe(http.MethodGet, "/api/v1/databases//pressure"),
	} {
		if skipsSessionAuth(req) {
			t.Errorf("%s: skipped session auth", name)
		}
	}
}

func TestPressureMCP_ServesTheSameCheck(t *testing.T) {
	src := &pressureSource{lag: 20, known: map[string]bool{"t": true}}
	b := PressureMCP(pressureFleet(t, src))
	out, err := b.CheckPressure(context.Background(), mcp.PressureRequest{Database: "orders",
		Table: "t", Job: "j", Batch: 10})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	v, ok := out.(pressure.Verdict)
	if !ok || v.Verdict != pressure.Stop || v.Database != "orders" || v.Job != "j" {
		t.Fatalf("MCP verdict = %#v, want stop for orders", out)
	}
	if _, err := b.CheckPressure(context.Background(), mcp.PressureRequest{
		Database: "nope"}); !errors.Is(err, mcp.ErrPressureDatabaseNotFound) {
		t.Fatalf("unknown database err = %v, want ErrPressureDatabaseNotFound", err)
	}
	unwired := PressureMCP(pressureFleet(t, nil))
	if _, err := unwired.CheckPressure(context.Background(), mcp.PressureRequest{
		Database: "orders"}); !errors.Is(err, pressure.ErrUnavailable) {
		t.Fatalf("unwired err = %v, want pressure.ErrUnavailable", err)
	}
}
