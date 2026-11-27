package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// readyTLS probes /ready and returns the status code and raw details.
func readyTLS(t *testing.T, probe ReadinessProbe) (int, map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	NewReadinessHandler(probe).ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestReadinessTLSExpiry_ExpiringIsADetailNotAFailure(t *testing.T) {
	p := okProbe()
	p.TLSExpiring = func() bool { return true }
	code, body := readyTLS(t, p)
	if code != http.StatusOK || string(body["status"]) != `"ready"` {
		t.Fatalf("expiring certificate failed readiness: %d %s", code, body["status"])
	}
	if got := string(body["details"]); got != `{"tls_expiring":true}` {
		t.Fatalf("details = %s, want tls_expiring true", got)
	}
}

func TestReadinessTLSExpiry_HealthyCertificateReportsFalse(t *testing.T) {
	p := okProbe()
	p.TLSExpiring = func() bool { return false }
	if _, body := readyTLS(t, p); string(body["details"]) != `{"tls_expiring":false}` {
		t.Fatalf("details = %s, want tls_expiring false", body["details"])
	}
}

// Nil: plain HTTP has no TLS detail at all.
func TestReadinessTLSExpiry_PlainHTTPHasNoDetails(t *testing.T) {
	if _, body := readyTLS(t, okProbe()); body["details"] != nil {
		t.Fatalf("plain HTTP details = %s, want none", body["details"])
	}
}

// The detail is still reported while another check fails readiness.
func TestReadinessTLSExpiry_ReportedWhileNotReady(t *testing.T) {
	p := okProbe()
	p.ControlDB = func(context.Context) error { return errors.New("down") }
	p.TLSExpiring = func() bool { return true }
	code, body := readyTLS(t, p)
	if code != http.StatusServiceUnavailable || string(body["details"]) != `{"tls_expiring":true}` {
		t.Fatalf("code %d details %s", code, body["details"])
	}
}

// The router wires RuntimeDeps.TLSExpiring into its /ready probe; without
// it (plain HTTP, or no runtime) /ready has no TLS detail.
func TestReadinessTLSExpiry_RouterWiresRuntimeDep(t *testing.T) {
	cfg := config.DefaultConfig()
	cases := []struct {
		name string
		rt   *RuntimeDeps
		want string
	}{
		{"expiring", &RuntimeDeps{TLSExpiring: func() bool { return true }},
			`{"tls_expiring":true}`},
		{"healthy", &RuntimeDeps{TLSExpiring: func() bool { return false }},
			`{"tls_expiring":false}`},
		{"plain HTTP", &RuntimeDeps{}, ""},
		{"no runtime", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestRouter(fleet.NewManager(cfg), cfg, nil, nil, nil, nil, tc.rt)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("/ready body %q: %v", rec.Body.String(), err)
			}
			if got := string(body["details"]); got != tc.want {
				t.Fatalf("/ready details = %q, want %q", got, tc.want)
			}
		})
	}
}
