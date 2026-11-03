package api

import (
	"bytes"
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
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

func appSLIRouter(mgr *fleet.DatabaseManager, cfg *config.Config, user *auth.User, platform bool) http.Handler {
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), userContextKey, user)
			if platform {
				ctx = context.WithValue(ctx, authModeContextKey, auth.DatabricksAppMode)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	return NewRouterFullRuntime(mgr, cfg, nil, nil, nil, nil, &RuntimeDeps{DatabricksAppAuth: true}, inject)
}

func appSLIRequest(h http.Handler, path, body string, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://sage.databricksapps.com"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAppSLISamplesRequirePlatformAdmin(t *testing.T) {
	cfg := config.DefaultConfig()
	path := "/api/v1/sre/slos/checkout/samples?database=orders"
	for _, tc := range []struct {
		name     string
		user     *auth.User
		platform bool
		code     int
	}{
		{"anonymous", nil, true, 401},
		{"viewer", &auth.User{ID: 3, Role: auth.RoleViewer}, true, 403},
		{"operator", &auth.User{ID: 3, Role: auth.RoleOperator}, true, 403},
		{"local administrator", &auth.User{ID: 3, Role: auth.RoleAdmin}, false, 403},
		{"nonpersisted administrator", &auth.User{Role: auth.RoleAdmin}, true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := appSLIRequest(appSLIRouter(fleet.NewManager(cfg), cfg, tc.user, tc.platform), path, `{}`, "")
			if w.Code != tc.code {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	h := appSLIRouter(fleet.NewManager(cfg), cfg, &auth.User{ID: 3, Role: auth.RoleAdmin}, true)
	if w := appSLIRequest(h, path, `{}`, "https://evil.example"); w.Code != 403 {
		t.Fatalf("cross-origin: %d", w.Code)
	}
	// Standalone never registers this platform-only ingress.
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey, &auth.User{ID: 3, Role: auth.RoleAdmin})))
		})
	}
	standalone := NewRouterFullRuntime(fleet.NewManager(cfg), cfg, nil, nil, nil, nil, nil, inject)
	if w := appSLIRequest(standalone, path, `{}`, ""); w.Code != 404 && w.Code != 405 {
		t.Fatalf("standalone route=%d", w.Code)
	}
}

func appSLIFixture(t *testing.T) (*fleet.DatabaseManager, *config.Config, signalDB) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.SRE.SLO.Enabled = true
	cfg.SRE.SLO.Push.HMACSecret = ""
	mgr := fleet.NewManager(cfg)
	db := signalInstance(t, mgr, "app-sli-"+fmt.Sprint(time.Now().UnixNano()))
	cfg.SRE.SLO.Objectives = []config.SREObjectiveConfig{{Name: "checkout-" + db.name, Database: db.name, Source: "push", Target: 0.999}}
	inst := mgr.GetInstance(db.name)
	bound, err := slo.NewEngine(slo.EngineDeps{Database: db.name, Objectives: db.slo.Objectives(),
		DatabaseScopedPush: true, Store: db.slo.Store(), Scope: inst.Investigations.Coordinator().Bind, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	db.slo, inst.SLO = bound, bound
	return mgr, cfg, db
}

func TestAppSLISamplesPersistReplayConflictAndActorBinding(t *testing.T) {
	mgr, cfg, db := appSLIFixture(t)
	user := &auth.User{ID: 87, Role: auth.RoleAdmin}
	h := appSLIRouter(mgr, cfg, user, true)
	name := "checkout-" + db.name
	path := "/api/v1/sre/slos/" + name + "/samples?database=" + db.name
	sample := slo.PushSample{Series: "web", Bad: 2, Eligible: 100, ObservedAt: time.Now().UTC()}
	raw, _ := json.Marshal(sample)
	for i, want := range []int{202, 200} {
		w := appSLIRequest(h, path, string(raw), "")
		if w.Code != want || !strings.Contains(w.Body.String(), fmt.Sprintf(`"duplicate":%t`, i > 0)) {
			t.Fatalf("replay %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	stored, ok, err := db.slo.ReadLastPushSample(context.Background(), name, "databricks_user:87:web")
	if err != nil || !ok || stored.Bad != 2 || stored.Eligible != 100 || !stored.ObservedAt.Equal(sample.ObservedAt.Truncate(time.Microsecond)) {
		t.Fatalf("durable actor sample=%+v ok=%v err=%v", stored, ok, err)
	}
	sample.Bad = 3
	raw, _ = json.Marshal(sample)
	if w := appSLIRequest(h, path, string(raw), ""); w.Code != 409 {
		t.Fatalf("changed replay=%d %s", w.Code, w.Body.String())
	}
	// A different persisted administrator owns an independent series.
	h = appSLIRouter(mgr, cfg, &auth.User{ID: 88, Role: auth.RoleAdmin}, true)
	if w := appSLIRequest(h, path, string(raw), ""); w.Code != 202 {
		t.Fatalf("second actor=%d %s", w.Code, w.Body.String())
	}
}

func TestAppSLISamplesRejectInvalidScopeAndBodies(t *testing.T) {
	mgr, cfg, db := appSLIFixture(t)
	name := "checkout-" + db.name
	h := appSLIRouter(mgr, cfg, &auth.User{ID: 87, Role: auth.RoleAdmin}, true)
	path := "/api/v1/sre/slos/" + name + "/samples?database=" + db.name
	for _, tc := range []struct {
		name, path, body string
		code             int
	}{
		{"missing database", "/api/v1/sre/slos/" + name + "/samples", `{}`, 400},
		{"multiple database", path + "&database=other", `{}`, 400},
		{"unknown database", path + "-other", `{}`, 404},
		{"unknown source", path, `{"source":"gateway"}`, 400},
		{"unknown actor", path, `{"actor":"admin"}`, 400},
		{"trailing body", path, `{} {}`, 400},
		{"oversized", path, string(bytes.Repeat([]byte("x"), maxSignedBody+1)), 413},
		{"empty timestamp", path, `{"bad":1,"eligible":10}`, 400},
		{"bad counters", path, fmt.Sprintf(`{"bad":11,"eligible":10,"observed_at":%q}`, time.Now().UTC().Format(time.RFC3339Nano)), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := appSLIRequest(h, tc.path, tc.body, "")
			if w.Code != tc.code {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
	cfg.SRE.SLO.Objectives[0].Database = ""
	if w := appSLIRequest(h, path, `{}`, ""); w.Code != 404 {
		t.Fatalf("unbound=%d", w.Code)
	}
	cfg.SRE.SLO.Objectives[0].Database = db.name
	cfg.SRE.SLO.Objectives[0].Source = "prometheus"
	if w := appSLIRequest(h, path, `{}`, ""); w.Code != 404 {
		t.Fatalf("external=%d", w.Code)
	}
	cfg.SRE.SLO.Objectives[0].Source = "push"
	// Simulate another registered database consuming this same durable sample key.
	other := *mgr.GetInstance(db.name)
	other.Name = "ambiguous"
	mgr.RegisterInstance(&other)
	if w := appSLIRequest(h, path, `{}`, ""); w.Code != 409 {
		t.Fatalf("ambiguous=%d %s", w.Code, w.Body.String())
	}
}
