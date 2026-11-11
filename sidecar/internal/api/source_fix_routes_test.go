package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// mcp-06: operators read a finding's source-fix report (stage, pull
// request, deploy, verdict) at GET /api/v1/findings/{id}/source-fix; it
// was readable only by the coding agent that filed it.

const sourceFixFinding = int64(2147470001)

func sourceFixConfig(windowMinutes int) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Verify.WindowMinutes = windowMinutes
	return cfg
}

// seedSourceFix stores a decided report for sourceFixFinding: deployed
// 3 h ago, reported 2.5 h after the deploy, improved by 62.5%.
func seedSourceFix(t *testing.T, ctx context.Context, pool *pgxpool.Pool) time.Time {
	t.Helper()
	deployed := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	_, err := pool.Exec(ctx, `INSERT INTO sage.source_fix (finding_id, packet_hash, stage,
		pr_url, commit_sha, reported_by, deployed_at, deploy_reported_at, prediction,
		target_queryids, verdict, tolerance, observed, evidence, reason, decided_at)
		VALUES ($1, 'sha256:abc', 'verified', 'https://github.com/acme/shop/pull/42',
		'0123abcd', 'agent:claude-code', $2::timestamptz, $2::timestamptz + interval '150 minutes',
		'{"method":"hypopg","expected_change_pct":-40,"target_queryids":[7]}', '{7}', 'improved',
		'met', '{"metric":"mean_exec_time","before":8,"after":3,"change_pct":-62.5}',
		'{"targets":[]}', 'mean latency fell', now())`, sourceFixFinding, deployed)
	if err != nil {
		t.Fatalf("seed source_fix: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.source_fix WHERE finding_id = $1", sourceFixFinding)
	})
	return deployed
}

func getSourceFix(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: decode %q: %v", path, w.Body.String(), err)
	}
	return w.Code, body
}

func sourceFixAPI(t *testing.T, cfg *config.Config, pools map[string]*pgxpool.Pool,
) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	registerSourceFixRoutes(mux, poolManager(pools), cfg)
	return routedAPI(cfg, mux)
}

// CHECK-06: the route's contract.
func TestSourceFixRoute_ReturnsTheStoredReport(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	deployed := seedSourceFix(t, ctx, pool)
	h := sourceFixAPI(t, sourceFixConfig(120), map[string]*pgxpool.Pool{"prod": pool})
	code, body := getSourceFix(t, h, "/api/v1/findings/2147470001/source-fix?database=prod")
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	want := map[string]any{"finding_id": float64(sourceFixFinding), "stage": "verified",
		"pr_url": "https://github.com/acme/shop/pull/42", "commit": "0123abcd",
		"verdict": "improved", "tolerance": "met", "observed_change_pct": -62.5,
		"backdated": true, "reason": "mean latency fell"}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %#v, want %#v", k, body[k], v)
		}
	}
	at, err := time.Parse(time.RFC3339, body["deployed_at"].(string))
	if err != nil || !at.Equal(deployed) {
		t.Errorf("deployed_at = %v (%v), want %s", body["deployed_at"], err, deployed)
	}
	after, err := time.Parse(time.RFC3339, body["verify_after"].(string))
	if err != nil || !after.Equal(deployed.Add(2*time.Hour)) {
		t.Errorf("verify_after = %v, want the deploy plus verify.window_minutes", after)
	}
	reported, err := time.Parse(time.RFC3339, body["deploy_reported_at"].(string))
	if err != nil || !reported.Equal(deployed.Add(150*time.Minute)) {
		t.Errorf("deploy_reported_at = %v (%v)", body["deploy_reported_at"], err)
	}
	pred, _ := body["prediction"].(map[string]any)
	if pred["method"] != "hypopg" || pred["expected_change_pct"] != float64(-40) {
		t.Errorf("prediction = %v, want the packet's", body["prediction"])
	}
}

// Backdated follows verify.window_minutes: reported 2.5 h after the
// deploy is backdated with a 2 h window, not with a 3 h one.
func TestSourceFixRoute_BackdatedFollowsTheWindow(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedSourceFix(t, ctx, pool)
	h := sourceFixAPI(t, sourceFixConfig(180), map[string]*pgxpool.Pool{"prod": pool})
	code, body := getSourceFix(t, h, "/api/v1/findings/2147470001/source-fix")
	if code != http.StatusOK || body["backdated"] != false {
		t.Fatalf("status %d backdated %v, want 200 and false with a 3 h window", code,
			body["backdated"])
	}
}

func TestSourceFixRoute_Errors(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedSourceFix(t, ctx, pool)
	one := sourceFixAPI(t, sourceFixConfig(120), map[string]*pgxpool.Pool{"prod": pool})
	two := sourceFixAPI(t, sourceFixConfig(120),
		map[string]*pgxpool.Pool{"prod": pool, "stage": pool})
	cases := []struct {
		name     string
		h        http.Handler
		path     string
		status   int
		wantCode string
	}{
		{"no report", one, "/api/v1/findings/2147470002/source-fix", 404, "no_source_fix"},
		{"non-numeric id", one, "/api/v1/findings/abc/source-fix", 400, ""},
		{"zero id", one, "/api/v1/findings/0/source-fix", 400, ""},
		{"negative id", one, "/api/v1/findings/-4/source-fix", 400, ""},
		{"overflowing id", one, "/api/v1/findings/99999999999999999999/source-fix", 400, ""},
		{"unknown database", one, "/api/v1/findings/2147470001/source-fix?database=nope",
			404, ""},
		{"bad database", one, "/api/v1/findings/2147470001/source-fix?database=a%20b",
			400, ""},
		{"fleet without database", two, "/api/v1/findings/2147470001/source-fix", 400, ""},
	}
	for _, c := range cases {
		code, body := getSourceFix(t, c.h, c.path)
		if code != c.status {
			t.Errorf("%s: status %d (%v), want %d", c.name, code, body, c.status)
		}
		if body["error"] == nil || body["error"] == "" {
			t.Errorf("%s: no error message: %v", c.name, body)
		}
		if c.wantCode != "" && body["code"] != c.wantCode {
			t.Errorf("%s: code %v, want %s", c.name, body["code"], c.wantCode)
		}
		if body["verdict"] != nil {
			t.Errorf("%s: error carries a report: %v", c.name, body)
		}
	}
	code, body := getSourceFix(t, two, "/api/v1/findings/2147470001/source-fix?database=stage")
	if code != http.StatusOK || body["verdict"] != "improved" {
		t.Fatalf("named database in fleet mode: %d %v", code, body)
	}
}

// A database whose pool fails is a 503 or 500, never a 404 that would read
// as "no report".
func TestSourceFixRoute_DatabaseFailureIsNotNotFound(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	closed, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	closed.Close()
	h := sourceFixAPI(t, sourceFixConfig(120), map[string]*pgxpool.Pool{"prod": closed})
	code, body := getSourceFix(t, h, "/api/v1/findings/2147470001/source-fix")
	if code != http.StatusServiceUnavailable && code != http.StatusInternalServerError {
		t.Fatalf("closed pool: status %d (%v), want 500 or 503", code, body)
	}
}

// Viewer role and up; the route is read-only.
func TestSourceFixRoute_GateIsViewerGET(t *testing.T) {
	mux := http.NewServeMux()
	registerSourceFixRoutes(mux, poolManager(nil), sourceFixConfig(120))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/findings/1/source-fix", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET: %d, want 401", w.Code)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, withUser(httptest.NewRequest(http.MethodPost,
		"/api/v1/findings/1/source-fix", nil), testAdminUser()))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d, want 405", w.Code)
	}
}
