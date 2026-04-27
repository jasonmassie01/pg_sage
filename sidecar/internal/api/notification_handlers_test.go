package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaskChannelSecretsMasksAllKnownSecretKeys(t *testing.T) {
	config := map[string]string{
		"webhook_url":   "https://hooks.slack.com/services/REAL/SECRET/TOKEN",
		"routing_key":   "pagerduty-routing-key",
		"smtp_pass":     "email-password",
		"smtp_password": "legacy-email-password",
		"api_key":       "api-key-secret",
		"token":         "token-secret",
		"secret":        "plain-secret",
		"to":            "alerts@example.com",
	}

	maskChannelSecrets(config)

	for _, key := range []string{
		"webhook_url", "routing_key", "smtp_pass",
		"smtp_password", "api_key", "token", "secret",
	} {
		if config[key] == "" || !strings.Contains(config[key], "****") {
			t.Fatalf("%s was not masked: %q", key, config[key])
		}
	}
	if config["to"] != "alerts@example.com" {
		t.Fatalf("non-secret value was changed: %q", config["to"])
	}
}

// The notification handlers all depend on *store.NotificationStore
// which is a concrete type backed by *pgxpool.Pool. We cannot mock
// the store without an interface. Tests here cover request parsing,
// validation, and path parameter extraction -- all code paths that
// execute before any store call.

// --- createChannelHandler ---

func TestCreateChannelHandler_MalformedJSON(t *testing.T) {
	handler := createChannelHandler(nil)

	w := doRequest(
		handler, "POST", "/api/v1/notifications/channels",
		"not json")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid request body" {
		t.Errorf("error: got %q, want 'invalid request body'",
			resp["error"])
	}
}

func TestCreateChannelHandler_MissingName(t *testing.T) {
	handler := createChannelHandler(nil)

	w := doRequest(
		handler, "POST", "/api/v1/notifications/channels",
		`{"type":"slack","config":{"webhook_url":"http://x"}}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "name and type required" {
		t.Errorf("error: got %q, want 'name and type required'",
			resp["error"])
	}
}

func TestCreateChannelHandler_MissingType(t *testing.T) {
	handler := createChannelHandler(nil)

	w := doRequest(
		handler, "POST", "/api/v1/notifications/channels",
		`{"name":"alerts"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "name and type required" {
		t.Errorf("error: got %q", resp["error"])
	}
}

func TestCreateChannelHandler_BothMissing(t *testing.T) {
	handler := createChannelHandler(nil)

	w := doRequest(
		handler, "POST", "/api/v1/notifications/channels",
		`{}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "name and type required" {
		t.Errorf("error: got %q", resp["error"])
	}
}

func TestCreateChannelHandler_EmptyStrings(t *testing.T) {
	handler := createChannelHandler(nil)

	w := doRequest(
		handler, "POST", "/api/v1/notifications/channels",
		`{"name":"","type":""}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
}

func TestCreateChannelHandler_ContentType(t *testing.T) {
	handler := createChannelHandler(nil)

	w := doRequest(
		handler, "POST", "/api/v1/notifications/channels",
		`{}`)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json",
			ct)
	}
}

// --- updateChannelHandler ---

func TestUpdateChannelHandler_InvalidID(t *testing.T) {
	handler := updateChannelHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/notifications/channels/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/notifications/channels/abc",
		strings.NewReader(`{"name":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid channel ID" {
		t.Errorf("error: got %q, want 'invalid channel ID'",
			resp["error"])
	}
}

func TestUpdateChannelHandler_MalformedJSON(t *testing.T) {
	handler := updateChannelHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/notifications/channels/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/notifications/channels/1",
		strings.NewReader("bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid request body" {
		t.Errorf("error: got %q, want 'invalid request body'",
			resp["error"])
	}
}

// --- deleteChannelHandler ---

func TestDeleteChannelHandler_InvalidID(t *testing.T) {
	handler := deleteChannelHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"DELETE /api/v1/notifications/channels/{id}", handler)

	req := httptest.NewRequest(
		"DELETE", "/api/v1/notifications/channels/abc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid channel ID" {
		t.Errorf("error: got %q, want 'invalid channel ID'",
			resp["error"])
	}
}

func TestDeleteChannelHandler_EmptyID(t *testing.T) {
	handler := deleteChannelHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"DELETE /api/v1/notifications/channels/{id}", handler)

	// With the Go 1.22+ mux, an empty path segment still
	// matches the pattern but the value will be empty.
	req := httptest.NewRequest(
		"DELETE", "/api/v1/notifications/channels/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Empty string fails strconv.Atoi, so expect 400 or 404
	// depending on mux routing.
	if w.Code != http.StatusBadRequest &&
		w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 400 or 404", w.Code)
	}
}

// --- testChannelHandler ---

func TestTestChannelHandler_InvalidID(t *testing.T) {
	handler := testChannelHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"POST /api/v1/notifications/channels/{id}/test",
		handler)

	req := httptest.NewRequest(
		"POST", "/api/v1/notifications/channels/xyz/test", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid channel ID" {
		t.Errorf("error: got %q, want 'invalid channel ID'",
			resp["error"])
	}
}

// --- createRuleHandler ---

func TestCreateRuleHandler_MalformedJSON(t *testing.T) {
	handler := createRuleHandler(nil)

	w := doRequest(
		handler, "POST", "/api/v1/notifications/rules",
		"not json")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid request body" {
		t.Errorf("error: got %q, want 'invalid request body'",
			resp["error"])
	}
}

func TestCreateRuleHandler_DefaultMinSeverity(t *testing.T) {
	// When min_severity is empty, handler defaults to "warning".
	// Handler then calls ns.CreateRule which needs a real store.
	// Nil store will panic — we recover to verify the handler
	// accepted the request (i.e., defaulted min_severity).
	defer func() {
		if r := recover(); r != nil {
			// Panic at store layer expected (nil store).
			// Reaching here means the handler accepted the input.
		}
	}()

	handler := createRuleHandler(nil)

	w := doRequest(
		handler, "POST", "/api/v1/notifications/rules",
		`{"channel_id":1,"event":"action_executed"}`)

	// Should not return 400 for invalid min_severity.
	if w.Code == http.StatusBadRequest {
		var resp map[string]string
		json.NewDecoder(w.Body).Decode(&resp)
		if strings.Contains(resp["error"], "severity") {
			t.Error("empty min_severity should default to " +
				"'warning', not be rejected")
		}
	}
}

// --- deleteRuleHandler ---

func TestDeleteRuleHandler_InvalidID(t *testing.T) {
	handler := deleteRuleHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"DELETE /api/v1/notifications/rules/{id}", handler)

	req := httptest.NewRequest(
		"DELETE", "/api/v1/notifications/rules/abc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid rule ID" {
		t.Errorf("error: got %q, want 'invalid rule ID'",
			resp["error"])
	}
}

// --- updateRuleHandler ---

func TestUpdateRuleHandler_InvalidID(t *testing.T) {
	handler := updateRuleHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/notifications/rules/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/notifications/rules/abc",
		strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid rule ID" {
		t.Errorf("error: got %q, want 'invalid rule ID'",
			resp["error"])
	}
}

func TestUpdateRuleHandler_MalformedJSON(t *testing.T) {
	handler := updateRuleHandler(nil)

	mux := http.NewServeMux()
	mux.HandleFunc(
		"PUT /api/v1/notifications/rules/{id}", handler)

	req := httptest.NewRequest(
		"PUT", "/api/v1/notifications/rules/1",
		strings.NewReader("bad json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "invalid request body" {
		t.Errorf("error: got %q, want 'invalid request body'",
			resp["error"])
	}
}

// --- listChannelsHandler ---
// Requires a real NotificationStore. No request validation to test.

// --- listRulesHandler ---
// Requires a real NotificationStore. No request validation to test.

// --- listNotificationLogHandler ---
// Requires a real NotificationStore. Only the limit param is
// parsed via parseIntDefault (tested in config_handlers_test.go).

// --- Table-driven: all handlers return JSON Content-Type on error ---

func TestNotificationHandlers_ErrorContentType(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		handler http.HandlerFunc
	}{
		{
			"createChannel bad JSON",
			"POST",
			"/api/v1/notifications/channels",
			"bad",
			createChannelHandler(nil),
		},
		{
			"createRule bad JSON",
			"POST",
			"/api/v1/notifications/rules",
			"bad",
			createRuleHandler(nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doRequest(tt.handler, tt.method, tt.path, tt.body)

			ct := w.Header().Get("Content-Type")
			if ct != "application/json" {
				t.Errorf("Content-Type: got %q, "+
					"want application/json", ct)
			}
		})
	}
}

// --- Table-driven: invalid ID on all handlers with path params ---

func TestNotificationHandlers_InvalidIDTableDriven(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		pattern string
		path    string
		body    string
		handler http.HandlerFunc
		errMsg  string
	}{
		{
			"updateChannel",
			"PUT",
			"PUT /api/v1/notifications/channels/{id}",
			"/api/v1/notifications/channels/abc",
			`{"name":"x"}`,
			updateChannelHandler(nil),
			"invalid channel ID",
		},
		{
			"deleteChannel",
			"DELETE",
			"DELETE /api/v1/notifications/channels/{id}",
			"/api/v1/notifications/channels/abc",
			"",
			deleteChannelHandler(nil),
			"invalid channel ID",
		},
		{
			"testChannel",
			"POST",
			"POST /api/v1/notifications/channels/{id}/test",
			"/api/v1/notifications/channels/abc/test",
			"",
			testChannelHandler(nil),
			"invalid channel ID",
		},
		{
			"deleteRule",
			"DELETE",
			"DELETE /api/v1/notifications/rules/{id}",
			"/api/v1/notifications/rules/abc",
			"",
			deleteRuleHandler(nil),
			"invalid rule ID",
		},
		{
			"updateRule",
			"PUT",
			"PUT /api/v1/notifications/rules/{id}",
			"/api/v1/notifications/rules/abc",
			`{"enabled":true}`,
			updateRuleHandler(nil),
			"invalid rule ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc(tt.pattern, tt.handler)

			var req *http.Request
			if tt.body != "" {
				req = httptest.NewRequest(
					tt.method, tt.path,
					strings.NewReader(tt.body))
				req.Header.Set(
					"Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(
					tt.method, tt.path, nil)
			}

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400",
					w.Code)
			}

			var resp map[string]string
			json.NewDecoder(w.Body).Decode(&resp)
			if resp["error"] != tt.errMsg {
				t.Errorf("error: got %q, want %q",
					resp["error"], tt.errMsg)
			}
		})
	}
}

// --- Table-driven: numeric IDs that parse but are unusual ---

func TestNotificationHandlers_NumericEdgeCaseIDs(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		method  string
		handler http.HandlerFunc
	}{
		{
			"deleteChannel zero",
			"DELETE /api/v1/notifications/channels/{id}",
			"/api/v1/notifications/channels/0",
			"DELETE",
			deleteChannelHandler(nil),
		},
		{
			"deleteChannel negative",
			"DELETE /api/v1/notifications/channels/{id}",
			"/api/v1/notifications/channels/-1",
			"DELETE",
			deleteChannelHandler(nil),
		},
		{
			"deleteRule zero",
			"DELETE /api/v1/notifications/rules/{id}",
			"/api/v1/notifications/rules/0",
			"DELETE",
			deleteRuleHandler(nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					// Panic at store layer expected (nil store).
					// Reaching here means the ID was accepted.
				}
			}()

			mux := http.NewServeMux()
			mux.HandleFunc(tt.pattern, tt.handler)

			req := httptest.NewRequest(
				tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			// These IDs parse as valid ints, so the handler
			// should NOT return "invalid ... ID". It will
			// proceed to the store call (which fails on nil).
			if w.Code == http.StatusBadRequest {
				var resp map[string]string
				json.NewDecoder(w.Body).Decode(&resp)
				if strings.Contains(
					resp["error"], "invalid") {
					t.Errorf("numeric ID should not be "+
						"rejected as invalid: %q",
						resp["error"])
				}
			}
		})
	}
}
