package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfconfig"
)

// Self-configuration API: GET /api/v1/derived-settings serves, per
// database, every derived key (value, evidence, bounds, status, history);
// POST .../{key}/pin and .../{key}/unpin are admin-only.

func cleanDerived(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DELETE FROM sage.config_derivation")
		_, _ = pool.Exec(ctx, "DELETE FROM sage.config_derived_setting")
	}
	clean()
	t.Cleanup(clean)
}

// seedDerivedSettings records one shadow candidate per key and one
// operator-set key, as a startup pass would.
func seedDerivedSettings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	e := selfconfig.NewEngine(selfconfig.NewStore(pool))
	e.Now = func() time.Time { return time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC) }
	cfg := config.DefaultConfig()
	cfg.Collector.IntervalSeconds = 45
	_, err := e.Reconcile(context.Background(), selfconfig.Input{Cfg: cfg,
		OperatorSet: map[string]bool{"collector.interval_seconds": true},
		Evidence: selfconfig.Evidence{CatalogScanMs: selfconfig.Known(300),
			MaxConnections: selfconfig.Known(1000)},
		Phase: selfconfig.PhaseStartup})
	if err != nil {
		t.Fatal(err)
	}
}

func derivedMux(pool *pgxpool.Pool) *http.ServeMux {
	mux := http.NewServeMux()
	registerDerivedSettingsRoutes(mux, phase2MgrWithPool(pool))
	return mux
}

func callDerived(t *testing.T, mux *http.ServeMux, method, path string,
	user *auth.User) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func settingsByKey(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	dbs, _ := body["databases"].([]any)
	if len(dbs) != 1 {
		t.Fatalf("databases %v", body)
	}
	db := dbs[0].(map[string]any)
	if db["database"] != "testdb" {
		t.Fatalf("database %v", db["database"])
	}
	out := map[string]map[string]any{}
	for _, s := range db["settings"].([]any) {
		row := s.(map[string]any)
		out[row["key"].(string)] = row
	}
	return out
}

func viewerUser() *auth.User { return &auth.User{ID: 3, Email: "v@test.com", Role: "viewer"} }

func TestDerivedSettingsListsEveryKeyWithEvidence(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cleanDerived(t, pool)
	seedDerivedSettings(t, pool)
	code, body := callDerived(t, derivedMux(pool), "GET",
		"/api/v1/derived-settings?database=testdb", viewerUser())
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if m, _ := body["meaning"].(string); !strings.Contains(m, "shadow") {
		t.Fatalf("meaning %q", body["meaning"])
	}
	rows := settingsByKey(t, body)
	if len(rows) != len(selfconfig.Rules()) {
		t.Fatalf("%d settings, want one per rule", len(rows))
	}
	qt := rows["safety.query_timeout_ms"]
	shadow, _ := qt["shadow"].(map[string]any)
	if qt["status"] != "shadow" || qt["value"] != 500.0 || shadow == nil ||
		shadow["value"] != 1200.0 || qt["class"] != "derivable" || qt["lifecycle"] != "restart" ||
		qt["default"] != 500.0 || qt["unit"] == "" || qt["rule_version"] != 1.0 {
		t.Fatalf("query timeout row %v", qt)
	}
	bounds, _ := qt["bounds"].(map[string]any)
	if bounds["min"] != 500.0 || bounds["max"] != 5000.0 {
		t.Fatalf("bounds %v", bounds)
	}
	ev, _ := qt["evidence"].([]any)
	if len(ev) == 0 || ev[0].(map[string]any)["name"] == "" {
		t.Fatalf("evidence %v", qt["evidence"])
	}
	hist, _ := qt["history"].([]any)
	if len(hist) != 1 || hist[0].(map[string]any)["event"] != "shadow" {
		t.Fatalf("history %v", qt["history"])
	}
	ci := rows["collector.interval_seconds"]
	if ci["status"] != "operator" || ci["value"] != 45.0 || ci["lifecycle"] != "reconfigure" {
		t.Fatalf("operator row %v", ci)
	}
	// A key with no evidence has a row and says why it is not derived.
	tf := rows["sre.detectors.temp_file_mb"]
	if tf["status"] != "default" || tf["value"] != 1024.0 || tf["note"] == "" {
		t.Fatalf("not derived row %v", tf)
	}
}

func TestDerivedSettingsBeforeAnyDerivation(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cleanDerived(t, pool)
	code, body := callDerived(t, derivedMux(pool), "GET", "/api/v1/derived-settings",
		viewerUser())
	if code != http.StatusOK {
		t.Fatalf("status %d %v", code, body)
	}
	rows := settingsByKey(t, body)
	if len(rows) != len(selfconfig.Rules()) {
		t.Fatalf("rows %v", rows)
	}
	for key, row := range rows {
		if row["status"] != "default" || row["note"] == "" {
			t.Errorf("%s before derivation: %v", key, row)
		}
	}
}

func TestDerivedSettingsPinAndUnpin(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cleanDerived(t, pool)
	seedDerivedSettings(t, pool)
	mux := derivedMux(pool)
	base := "/api/v1/derived-settings/safety.query_timeout_ms/"
	code, body := callDerived(t, mux, "POST", base+"pin?database=testdb", testAdminUser())
	if code != http.StatusOK || body["status"] != "pinned" || body["value"] != 500.0 {
		t.Fatalf("pin %d %v", code, body)
	}
	pinned, _ := body["pinned"].(map[string]any)
	if pinned == nil || pinned["by"] != "admin@test.com" || pinned["value"] != 500.0 {
		t.Fatalf("pinned %v", body["pinned"])
	}
	code, body = callDerived(t, mux, "POST", base+"unpin", testAdminUser())
	if code != http.StatusOK || body["status"] == "pinned" || body["pinned"] != nil {
		t.Fatalf("unpin %d %v", code, body)
	}
	code, _ = callDerived(t, mux, "POST", base+"unpin", testAdminUser())
	if code != http.StatusConflict {
		t.Fatalf("unpin of an unpinned key: %d", code)
	}
}

func TestDerivedSettingsPinErrors(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cleanDerived(t, pool)
	seedDerivedSettings(t, pool)
	mux := derivedMux(pool)
	for name, tc := range map[string]struct {
		method, path string
		user         *auth.User
		want         int
	}{
		"viewer cannot pin": {"POST", "/api/v1/derived-settings/safety.query_timeout_ms/pin",
			viewerUser(), http.StatusForbidden},
		"operator role cannot pin": {"POST",
			"/api/v1/derived-settings/safety.query_timeout_ms/pin", testOperatorUser(),
			http.StatusForbidden},
		"anonymous": {"GET", "/api/v1/derived-settings", nil, http.StatusUnauthorized},
		"not a derived key": {"POST", "/api/v1/derived-settings/trust.level/pin",
			testAdminUser(), http.StatusNotFound},
		"operator-set key": {"POST",
			"/api/v1/derived-settings/collector.interval_seconds/pin", testAdminUser(),
			http.StatusConflict},
		"unknown database": {"POST",
			"/api/v1/derived-settings/safety.query_timeout_ms/pin?database=nope",
			testAdminUser(), http.StatusNotFound},
		"bad database param": {"GET", "/api/v1/derived-settings?database=" +
			strings.Repeat("x", 300), viewerUser(), http.StatusBadRequest},
	} {
		code, body := callDerived(t, mux, tc.method, tc.path, tc.user)
		if code != tc.want {
			t.Errorf("%s: %d %v, want %d", name, code, body, tc.want)
		}
	}
}

func TestDerivedSettingsPinBeforeDerivation(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cleanDerived(t, pool)
	code, body := callDerived(t, derivedMux(pool), "POST",
		"/api/v1/derived-settings/safety.query_timeout_ms/pin", testAdminUser())
	if code != http.StatusConflict || !strings.Contains(body["error"].(string), "not derived") {
		t.Fatalf("pin before derivation: %d %v", code, body)
	}
}
