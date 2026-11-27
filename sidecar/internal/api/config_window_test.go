package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

// Wednesday 2026-11-11 12:00 UTC (06:00 in Chicago).
func fixedWindowClock() time.Time {
	return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
}

type windowPreviewBody struct {
	Valid     bool    `json:"valid"`
	Disabled  bool    `json:"disabled"`
	OpenNow   bool    `json:"open_now"`
	ClosesAt  *string `json:"closes_at"`
	NextOpen  *string `json:"next_open"`
	NextClose *string `json:"next_close"`
	Horizon   int     `json:"horizon_hours"`
	Error     string  `json:"error"`
}

func postValidateWindow(t *testing.T, body string) (int, windowPreviewBody) {
	t.Helper()
	handler := validateWindowHandler(fixedWindowClock)
	w := doRequest(handler, http.MethodPost, "/api/v1/config/validate-window", body)
	var decoded windowPreviewBody
	if err := json.NewDecoder(w.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode %d response: %v", w.Code, err)
	}
	return w.Code, decoded
}

func stringOrNone(value *string) string {
	if value == nil {
		return "<null>"
	}
	return *value
}

// Invalid input: a four-field cron is refused with the parser's message.
func TestValidateWindow_RejectsShortCron(t *testing.T) {
	code, body := postValidateWindow(t, `{"expression":"0 2 * *"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	for _, want := range []string{`window "0 2 * *"`, "is not a recognized window",
		"expected always", "or never to disable"} {
		if !strings.Contains(body.Error, want) {
			t.Errorf("error %q lacks %q", body.Error, want)
		}
	}
}

func TestValidateWindow_WeeknightsChicagoNextOpening(t *testing.T) {
	code, body := postValidateWindow(t,
		`{"expression":"weeknights America/Chicago"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %+v", code, body)
	}
	if !body.Valid || body.Disabled || body.OpenNow {
		t.Fatalf("body = %+v, want valid, enabled and closed now", body)
	}
	if got := stringOrNone(body.NextOpen); got != "2026-11-11T22:00:00-06:00" {
		t.Errorf("next_open = %s, want 2026-11-11T22:00:00-06:00", got)
	}
	if got := stringOrNone(body.NextClose); got != "2026-11-12T06:00:00-06:00" {
		t.Errorf("next_close = %s, want 2026-11-12T06:00:00-06:00", got)
	}
	if body.ClosesAt != nil {
		t.Errorf("closes_at = %s, want null while closed", *body.ClosesAt)
	}
	if body.Horizon != 192 {
		t.Errorf("horizon_hours = %d, want 192", body.Horizon)
	}
}

// Boundary: a range that wraps midnight opens on its start day.
func TestValidateWindow_WrapsMidnightOnStartDay(t *testing.T) {
	code, body := postValidateWindow(t, `{"expression":"23:00-01:00"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %+v", code, body)
	}
	if got := stringOrNone(body.NextOpen); got != "2026-11-11T23:00:00Z" {
		t.Errorf("next_open = %s, want 2026-11-11T23:00:00Z", got)
	}
	if got := stringOrNone(body.NextClose); got != "2026-11-12T01:00:00Z" {
		t.Errorf("next_close = %s, want 2026-11-12T01:00:00Z", got)
	}
}

func TestValidateWindow_OpenNow(t *testing.T) {
	code, body := postValidateWindow(t, `{"expression":"always"}`)
	if code != http.StatusOK || !body.OpenNow {
		t.Fatalf("status %d body %+v, want 200 and open now", code, body)
	}
	if body.ClosesAt != nil || body.NextOpen != nil {
		t.Errorf("always closes_at=%s next_open=%s, want null for both",
			stringOrNone(body.ClosesAt), stringOrNone(body.NextOpen))
	}
}

// Nil/empty: unset and the never aliases are valid and closed, exactly as
// trust.maintenance_window treats them.
func TestValidateWindow_UnsetAndNeverAreDisabled(t *testing.T) {
	for _, expression := range []string{"", "  ", "never", "OFF"} {
		code, body := postValidateWindow(t,
			`{"expression":`+jsonQuote(expression)+`}`)
		if code != http.StatusOK || !body.Valid || !body.Disabled || body.OpenNow {
			t.Errorf("%q: status %d body %+v, want valid and disabled",
				expression, code, body)
		}
		if body.NextOpen != nil {
			t.Errorf("%q: next_open = %s, want null", expression, *body.NextOpen)
		}
	}
}

func TestValidateWindow_NoOpeningWithinHorizon(t *testing.T) {
	code, body := postValidateWindow(t, `{"expression":"0 2 29 2 *"}`)
	if code != http.StatusOK || !body.Valid || body.OpenNow {
		t.Fatalf("status %d body %+v, want valid and closed", code, body)
	}
	if body.NextOpen != nil {
		t.Errorf("next_open = %s, want null beyond the horizon", *body.NextOpen)
	}
}

func TestValidateWindow_RejectsMalformedRequests(t *testing.T) {
	long := strings.Repeat("a", maxWindowExpressionLen+1)
	for name, body := range map[string]string{
		"invalid JSON":  `{"expression":`,
		"wrong type":    `{"expression":7}`,
		"unknown zone":  `{"expression":"weeknights Mars/Olympus"}`,
		"too long":      `{"expression":"` + long + `"}`,
		"bad clock":     `{"expression":"25:00-01:00"}`,
		"trailing data": `{"expression":"always"} {"x":1}`,
	} {
		code, decoded := postValidateWindow(t, body)
		if code != http.StatusBadRequest || decoded.Error == "" {
			t.Errorf("%s: status %d error %q, want 400 with a message",
				name, code, decoded.Error)
		}
	}
}

// The route is admin-only and served in both config modes; YAML fleet mode
// has no writes, but a preview writes nothing.
func TestValidateWindow_RouteIsAdminOnlyInBothModes(t *testing.T) {
	cfg := config.DefaultConfig()
	controller := config.NewConfigController(cfg, nil)
	for _, readOnly := range []bool{true, false} {
		mux := http.NewServeMux()
		registerConfigRoutesRuntime(
			mux, nil, cfg, nil, controller, staticConfigBase(cfg), readOnly,
		)
		body := `{"expression":"weeknights"}`
		admin := &auth.User{ID: 1, Role: "admin"}
		got := wave5ConfigRequest(mux, admin, http.MethodPost,
			"/api/v1/config/validate-window", body)
		if got.Code != http.StatusOK {
			t.Errorf("read_only=%v admin status = %d, want 200: %s",
				readOnly, got.Code, got.Body.String())
		}
		viewer := &auth.User{ID: 2, Role: "viewer"}
		got = wave5ConfigRequest(mux, viewer, http.MethodPost,
			"/api/v1/config/validate-window", body)
		if got.Code != http.StatusForbidden {
			t.Errorf("read_only=%v viewer status = %d, want 403", readOnly, got.Code)
		}
	}
}

func jsonQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
