package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/specialist"
)

// The Postgres-specialist contract is mounted under /api/v1/specialist/ and
// authenticates with MCP tokens only: the session middleware leaves the
// prefix to the contract's own bearer check. Its request audit (which
// agent asked what) is read by people in the dashboard.

type specialistStub struct{ paths []string }

func (s *specialistStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"contract_version":"pg_sage.specialist.v1"}`))
}

type auditStub struct {
	records []specialist.Record
	err     error
	limit   int
}

func (a *auditStub) Recent(_ context.Context, limit int) ([]specialist.Record, error) {
	a.limit = limit
	return a.records, a.err
}

func specialistRouter(t *testing.T, rt *RuntimeDeps, user *auth.User,
	session bool) http.Handler {
	t.Helper()
	var mws []func(http.Handler) http.Handler
	if session {
		mws = append(mws, SessionAuthMiddleware(nil))
	}
	mws = append(mws, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user != nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	})
	return NewRouterFullRuntime(nil, config.DefaultConfig(), nil, nil, nil, nil, rt, mws...)
}

func TestSpecialistContractIsLeftToItsBearerCheck(t *testing.T) {
	stub := &specialistStub{}
	h := specialistRouter(t, &RuntimeDeps{Specialist: stub}, nil, true)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/specialist/contract", ""},
		{"POST", "/api/v1/specialist/databases/orders/investigations",
			`{"symptom":{"summary":"x"}}`},
		{"POST", "/api/v1/specialist/adapters/pagerduty", `{"event":{}}`},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if len(stub.paths) != 3 {
		t.Fatalf("stub saw %v", stub.paths)
	}
	// Only the prefix is exempt from the session check.
	req := httptest.NewRequest("GET", "/api/v1/specialist-requests", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("the audit route needs a session: %d", w.Code)
	}
}

func TestSpecialistDisabledIsNotMounted(t *testing.T) {
	h := specialistRouter(t, &RuntimeDeps{}, testAdminUser(), false)
	req := httptest.NewRequest("GET", "/api/v1/specialist/contract", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled contract answered %d", w.Code)
	}
}

func TestSpecialistAuditRoute(t *testing.T) {
	audit := &auditStub{records: []specialist.Record{{ID: "r1", Kind: specialist.KindOpen,
		IdentityName: "PagerDuty", Actor: "agent:pagerduty:t-1", Transport: "pagerduty",
		Database: "orders", InvestigationID: "44444444-4444-4444-8444-444444444444",
		Created: true, Match: "new", Symptom: &specialist.Symptom{Summary: "caller text"},
		CreatedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}}}
	rt := &RuntimeDeps{Specialist: &specialistStub{}, SpecialistAudit: audit}
	viewer := specialistRouter(t, rt, testViewerUser(), false)
	w := httptest.NewRecorder()
	viewer.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/specialist-requests", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer: %d", w.Code)
	}
	op := specialistRouter(t, rt, testOperatorUser(), false)
	w = httptest.NewRecorder()
	op.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/specialist-requests?limit=5", nil))
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Items) != 1 ||
		body.Items[0]["identity_name"] != "PagerDuty" || audit.limit != 5 {
		t.Fatalf("operator: %d %s (limit %d)", w.Code, w.Body.String(), audit.limit)
	}
	if strings.Contains(w.Body.String(), "caller text") {
		t.Fatal("the audit list shows who asked what, not the caller's free text")
	}
	for _, bad := range []string{"0", "-1", "501", "x"} {
		w = httptest.NewRecorder()
		op.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/specialist-requests?limit="+bad,
			nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("limit %s: %d", bad, w.Code)
		}
	}
	audit.err = errors.New("connection refused")
	w = httptest.NewRecorder()
	op.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/specialist-requests", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("store outage: %d", w.Code)
	}
}
