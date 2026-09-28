package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUpdateRuleHandler_MissingEnabledIsRejected is the G7-B29
// regression: a body without "enabled" silently disabled the rule.
func TestUpdateRuleHandler_MissingEnabledIsRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/notifications/rules/{id}",
		updateRuleHandler(nil))
	for _, body := range []string{`{}`, `{"min_severity":"info"}`} {
		req := httptest.NewRequest("PUT", "/api/v1/notifications/rules/1",
			strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, w.Code)
		}
		var resp map[string]string
		_ = json.NewDecoder(w.Body).Decode(&resp)
		if !strings.Contains(resp["error"], "enabled") {
			t.Fatalf("error = %q, want mention of enabled", resp["error"])
		}
	}
}

// TestDefaultRuleSeverity_MatchesEvent is the API half of G7-B06: the
// handler defaulted every rule to "warning", which info events never
// reach.
func TestDefaultRuleSeverity_MatchesEvent(t *testing.T) {
	cases := map[string]string{
		"action_executed":  "info",
		"action_failed":    "warning",
		"finding_critical": "critical",
	}
	for event, want := range cases {
		if got := defaultRuleSeverity(event, ""); got != want {
			t.Errorf("defaultRuleSeverity(%s) = %q, want %q", event, got, want)
		}
	}
	if got := defaultRuleSeverity("action_executed", "critical"); got != "critical" {
		t.Errorf("explicit severity overridden: %q", got)
	}
}
