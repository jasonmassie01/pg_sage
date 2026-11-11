package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// tuning-10: POST /api/v1/explain resolves query_id per database; an id
// with no statement in the named database is a 400 the caller can act on.

func explainQueryIDHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	cfg := &config.Config{Mode: "fleet",
		Explain: config.ExplainConfig{Enabled: true, TimeoutMs: 5000}}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "primary", Pool: pool,
		Status: &fleet.InstanceStatus{Connected: true}})
	return explainHandler(mgr, cfg, nil)
}

func postExplain(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/explain?database=primary",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestExplainHandler_UnknownQueryIDIs400(t *testing.T) {
	h := explainQueryIDHandler(t)
	for _, body := range []string{`{"query_id": 4242424242424242}`,
		`{"query_id": "-4242424242424242"}`} {
		w := postExplain(h, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400; body=%s", body, w.Code, w.Body.String())
		}
		var resp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if strings.Contains(resp["error"], "not yet implemented") || resp["error"] == "" {
			t.Errorf("error = %q, want why the id cannot be explained", resp["error"])
		}
	}
}

// Invalid input: a query id that is not an exact 64-bit integer is refused
// before any database work.
func TestExplainHandler_InexactQueryIDIs400(t *testing.T) {
	h := explainQueryIDHandler(t)
	w := postExplain(h, `{"query_id": 4.2}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
