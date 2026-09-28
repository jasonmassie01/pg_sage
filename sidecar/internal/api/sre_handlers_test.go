package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sage SRE investigation routes (AI-SRE-SPEC §9): viewers list and read,
// operators export and pin (CHECK-25); every route is scoped to the
// named database (CHECK-09/26); errors use canonical codes.

type lockChainRunner struct{}

func (lockChainRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	res := probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
		ObservedAt: time.Now()}
	if id == probes.LockGraph {
		res.Status = probes.StatusOK
		res.Rows = []probes.Row{{"waiter_pid": int64(20), "lock_type": "relation",
			"requested_mode": "AccessExclusiveLock", "relation": "public.orders",
			"blocker_pid": int64(4242), "blocker_kind": "backend",
			"blocker_state": "idle in transaction", "blocker_waiting": false,
			"blocker_xact_age_s":    90.0,
			"blocker_backend_start": time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)}}
	}
	return res
}

// sreInstance registers a database whose investigator has one concluded
// lock investigation.
func sreInstance(t *testing.T, mgr *fleet.DatabaseManager, name string) sre.Investigation {
	t.Helper()
	pool := surfacePool(t)
	ctx := context.Background()
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	cc := sre.DefaultCoordinatorConfig(fmt.Sprintf("api-test:%s:%d", name,
		time.Now().UnixNano()))
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st,
		Runner: lockChainRunner{}, Config: cc})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv, _, err := coord.Start(ctx, sre.Trigger{CaseID: "incident:" + name + ":lock:1",
		Kind: sre.TriggerLock, Subject: "incident 1", IdempotencyKey: "incident:1"})
	if err != nil || coord.Investigate(ctx, inv.ID) != nil {
		t.Fatalf("investigation: %v", err)
	}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
		Status: &fleet.InstanceStatus{}, Investigations: sre.NewService(name, coord, st)})
	return inv
}

func sreRouter(t *testing.T, mgr *fleet.DatabaseManager, user *auth.User) http.Handler {
	t.Helper()
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user != nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(mgr, config.DefaultConfig(), nil, nil, nil, nil, nil, inject)
}

func sreCall(t *testing.T, h http.Handler, method, path string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Body = http.NoBody
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String(), w.Header().Get("Content-Type")
}

func sreFixture(t *testing.T) (*fleet.DatabaseManager, sre.Investigation, sre.Investigation) {
	t.Helper()
	mgr := fleet.NewManager(config.DefaultConfig())
	return mgr, sreInstance(t, mgr, "orders"), sreInstance(t, mgr, "billing")
}

func TestSREAPI_ViewerListsAndReads(t *testing.T) {
	mgr, orders, _ := sreFixture(t)
	h := sreRouter(t, mgr, testViewerUser())
	code, body, _ := sreCall(t, h, "GET", "/api/v1/databases/orders/investigations")
	var list struct {
		Database string           `json:"database"`
		Items    []map[string]any `json:"items"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil ||
		list.Database != "orders" || len(list.Items) != 1 ||
		list.Items[0]["id"] != string(orders.ID) || list.Items[0]["state"] != "concluded" {
		t.Fatalf("list = %d %s", code, body)
	}
	code, body, _ = sreCall(t, h, "GET",
		"/api/v1/databases/orders/investigations/"+string(orders.ID))
	var detail sre.Detail
	if code != 200 || json.Unmarshal([]byte(body), &detail) != nil ||
		len(detail.Hypotheses) == 0 || !detail.ChainVerified ||
		detail.Hypotheses[0].Node != "idle_in_tx_holder" {
		t.Fatalf("detail = %d %s", code, body)
	}
	ev := detail.Evidence[0].ID
	code, body, _ = sreCall(t, h, "GET", fmt.Sprintf(
		"/api/v1/databases/orders/investigations/%s/evidence/%s", orders.ID, ev))
	if code != 200 || !strings.Contains(body, string(ev)) {
		t.Fatalf("evidence = %d %s", code, body)
	}
	code, body, _ = sreCall(t, h, "GET",
		"/api/v1/databases/orders/investigations/"+string(orders.ID)+"/events")
	if code != 200 || !strings.Contains(body, `"chain_verified":true`) {
		t.Fatalf("events = %d %s", code, body)
	}
}

// CHECK-09/26: an id of another database is not found under this one,
// in every read route.
func TestSREAPI_RoutesAreScopedToTheNamedDatabase(t *testing.T) {
	mgr, orders, billing := sreFixture(t)
	h := sreRouter(t, mgr, testOperatorUser())
	for _, path := range []string{
		"/api/v1/databases/orders/investigations/" + string(billing.ID),
		"/api/v1/databases/orders/investigations/" + string(billing.ID) + "/events",
		"/api/v1/databases/orders/investigations/" + string(billing.ID) + "/export",
		"/api/v1/databases/billing/investigations/" + string(orders.ID),
	} {
		if code, body, _ := sreCall(t, h, "GET", path); code != 404 ||
			!strings.Contains(body, "not_found") {
			t.Errorf("GET %s = %d %s, want 404 not_found", path, code, body)
		}
	}
	for path, want := range map[string]int{
		"/api/v1/databases/nope/investigations":                                       404,
		"/api/v1/databases/orders/investigations/not-a-uuid":                          400,
		"/api/v1/databases/orders/investigations?limit=-2":                            400,
		"/api/v1/databases/orders/investigations?limit=abc":                           400,
		"/api/v1/databases/orders/investigations?cursor=zzz":                          400,
		"/api/v1/databases/bad%20name/investigations":                                 400,
		"/api/v1/investigations?database=nope":                                        404,
		"/api/v1/databases/orders/investigations?case_id=" + strings.Repeat("x", 300): 400,
	} {
		if code, body, _ := sreCall(t, h, "GET", path); code != want {
			t.Errorf("GET %s = %d %s, want %d", path, code, body, want)
		}
	}
}

// CHECK-25: viewers cannot export or pin; operators can, attributed.
func TestSREAPI_ExportAndPinNeedAnOperator(t *testing.T) {
	mgr, orders, _ := sreFixture(t)
	base := "/api/v1/databases/orders/investigations/" + string(orders.ID)
	viewer := sreRouter(t, mgr, testViewerUser())
	for _, c := range []struct{ method, path string }{{"GET", base + "/export"},
		{"POST", base + "/pin"}, {"POST", base + "/unpin"}} {
		if code, _, _ := sreCall(t, viewer, c.method, c.path); code != 403 {
			t.Errorf("viewer %s %s = %d, want 403", c.method, c.path, code)
		}
	}
	op := sreRouter(t, mgr, testOperatorUser())
	code, body, _ := sreCall(t, op, "GET", base+"/export")
	if code != 200 || !strings.Contains(body, sre.ExportSchemaVersion) {
		t.Fatalf("export = %d %s", code, body)
	}
	code, body, ctype := sreCall(t, op, "GET", base+"/export?format=markdown")
	if code != 200 || !strings.HasPrefix(ctype, "text/markdown") ||
		!strings.Contains(body, "# Investigation") {
		t.Fatalf("markdown export = %d %s %s", code, ctype, body)
	}
	if code, _, _ := sreCall(t, op, "GET", base+"/export?format=pdf"); code != 400 {
		t.Fatalf("unknown export format = %d, want 400", code)
	}
	code, body, _ = sreCall(t, op, "POST", base+"/pin")
	if code != 200 || !strings.Contains(body, `"pinned":true`) {
		t.Fatalf("pin = %d %s", code, body)
	}
	_, body, _ = sreCall(t, op, "GET", base+"/events")
	if !strings.Contains(body, `"actor":"user:2"`) {
		t.Fatalf("pin event not attributed to the operator: %s", body)
	}
}

func TestSREAPI_UnauthenticatedAndUnavailable(t *testing.T) {
	mgr, _, _ := sreFixture(t)
	if code, _, _ := sreCall(t, sreRouter(t, mgr, nil), "GET",
		"/api/v1/databases/orders/investigations"); code != 401 {
		t.Fatalf("unauthenticated list = %d, want 401", code)
	}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "bare",
		Status: &fleet.InstanceStatus{}})
	code, body, _ := sreCall(t, sreRouter(t, mgr, testViewerUser()), "GET",
		"/api/v1/databases/bare/investigations")
	if code != 503 || !strings.Contains(body, "metadata_unavailable") {
		t.Fatalf("database without an investigator = %d %s, want 503", code, body)
	}
}

// The dashboard reads every database's investigations in one call, each
// tagged with its database, and can filter by case.
func TestSREAPI_FleetListForTheCasesPanel(t *testing.T) {
	mgr, orders, billing := sreFixture(t)
	h := sreRouter(t, mgr, testViewerUser())
	code, body, _ := sreCall(t, h, "GET", "/api/v1/investigations?database=all")
	if code != 200 || !strings.Contains(body, string(orders.ID)) ||
		!strings.Contains(body, string(billing.ID)) ||
		!strings.Contains(body, `"database":"billing"`) {
		t.Fatalf("fleet list = %d %s", code, body)
	}
	code, body, _ = sreCall(t, h, "GET", "/api/v1/investigations?database=orders")
	if code != 200 || strings.Contains(body, string(billing.ID)) {
		t.Fatalf("orders list = %d %s", code, body)
	}
	code, body, _ = sreCall(t, h, "GET",
		"/api/v1/investigations?database=all&case_id=incident:orders:lock:1")
	if code != 200 || !strings.Contains(body, string(orders.ID)) ||
		strings.Contains(body, string(billing.ID)) {
		t.Fatalf("case filter = %d %s", code, body)
	}
}
