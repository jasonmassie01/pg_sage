package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/codehost/codehosttest"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The call-site route (pr-01a) without a database: request validation and
// the answer when no code host is configured. No concurrent access tests:
// the handler keeps no state; the code host's cap is tested in codehost.

func callSiteMux(mgr *fleet.DatabaseManager, src *CallSiteSource) *http.ServeMux {
	mux := http.NewServeMux()
	registerCallSiteRoutes(mux, mgr, src)
	return mux
}

func TestCallSiteRoute_InvalidInputIs400(t *testing.T) {
	mux := callSiteMux(noPoolMgr(), &CallSiteSource{Host: codehosttest.New(),
		Repo: "acme/shop"})
	for name, path := range map[string]string{
		"queryid text":     "/api/v1/queries/abc/call-site?database=alpha",
		"no database":      "/api/v1/queries/42/call-site",
		"all databases":    "/api/v1/queries/42/call-site?database=all",
		"bad database":     "/api/v1/queries/42/call-site?database=a%3Bdrop",
		"queryid overflow": "/api/v1/queries/99999999999999999999/call-site?database=alpha",
	} {
		code, body := getWorkload(t, mux, path)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status %d (%v), want 400", name, code, body)
		}
	}
	code, body := getWorkload(t, mux, "/api/v1/queries/42/call-site?database=nope")
	if code != http.StatusNotFound {
		t.Errorf("unknown database: status %d (%v), want 404", code, body)
	}
}

func TestCallSiteRoute_OffWithoutACodeHost(t *testing.T) {
	for name, src := range map[string]*CallSiteSource{"nil source": nil,
		"nil host": {Repo: "acme/shop"}} {
		mux := callSiteMux(noPoolMgr(), src)
		code, body := getWorkload(t, mux, "/api/v1/queries/42/call-site?database=alpha")
		if code != http.StatusOK {
			t.Fatalf("%s: status %d (%v), want 200", name, code, body)
		}
		if body["configured"] != false || body["queryid"] != "42" ||
			body["database"] != "alpha" {
			t.Errorf("%s: body %v, want configured=false for alpha/42", name, body)
		}
		if note, _ := body["note"].(string); !strings.Contains(note, "agents.code_host") {
			t.Errorf("%s: note %q does not say how to configure it", name, note)
		}
		if _, ok := body["call_site"]; ok {
			t.Errorf("%s: call_site present while off: %v", name, body)
		}
	}
}

func TestCallSiteRoute_DisconnectedDatabaseSaysSo(t *testing.T) {
	host := codehosttest.New()
	mux := callSiteMux(noPoolMgr(), &CallSiteSource{Host: host, Repo: "acme/shop"})
	code, body := getWorkload(t, mux, "/api/v1/queries/42/call-site?database=alpha")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d (%v), want 503", code, body)
	}
	if host.Calls() != 0 {
		t.Errorf("code host called %d times for a database that is not connected",
			host.Calls())
	}
}

func TestCallSiteErrorStatus(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{fmt.Errorf("x: %w", codehost.ErrForbidden), http.StatusBadGateway},
		{fmt.Errorf("x: %w", codehost.ErrUnavailable), http.StatusBadGateway},
		{&codehost.RateLimitError{Host: "h", RetryAfter: 42e9}, http.StatusServiceUnavailable},
		{fmt.Errorf("x: %w", agenttools.ErrNotFound), http.StatusNotFound},
		{fmt.Errorf("x: %w", agenttools.ErrInvalid), http.StatusUnprocessableEntity},
		{errors.New("connection reset by peer at 10.0.0.5:5432"),
			http.StatusInternalServerError},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		writeCallSiteError(w, httptest.NewRequest(http.MethodGet, "/", nil), tc.err)
		if w.Code != tc.code {
			t.Errorf("%v: status %d, want %d", tc.err, w.Code, tc.code)
		}
		if tc.code == http.StatusInternalServerError &&
			strings.Contains(w.Body.String(), "10.0.0.5") {
			t.Errorf("database error text reached the client: %s", w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	writeCallSiteError(w, httptest.NewRequest(http.MethodGet, "/", nil),
		&codehost.RateLimitError{Host: "h", RetryAfter: 42e9})
	if got := w.Header().Get("Retry-After"); got != "42" {
		t.Errorf("Retry-After = %q, want 42", got)
	}
}

func TestCallSiteRetryAfterRoundsUpToWholeSeconds(t *testing.T) {
	// A cool-down with a fraction of a second left must not answer
	// "Retry-After: 0", which invites an immediate retry the client refuses.
	for _, tc := range []struct {
		left time.Duration
		want string
	}{{400 * time.Millisecond, "1"}, {41200 * time.Millisecond, "42"}, {42 * time.Second, "42"}} {
		w := httptest.NewRecorder()
		writeCallSiteError(w, httptest.NewRequest(http.MethodGet, "/", nil),
			&codehost.RateLimitError{Host: "h", RetryAfter: tc.left})
		if got := w.Header().Get("Retry-After"); got != tc.want {
			t.Errorf("%s left: Retry-After = %q, want %q", tc.left, got, tc.want)
		}
	}
}
