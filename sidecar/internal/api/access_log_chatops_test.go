package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// No concurrency test: the callback's entry lives on one request goroutine.

// Signed ChatOps callbacks are mounted on the root mux, outside the API
// handler chain, and they decide actions: they are access-logged like any
// other mutating API request, with the route pattern and no session user.
func TestAccessLog_ChatOpsCallbackIsLogged(t *testing.T) {
	off := false
	cases := []struct {
		name      string
		accessLog *bool
		wantLines int
	}{
		{"default on", nil, 1},
		{"access_log false", &off, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.API.AccessLog = tc.accessLog
			h := newTestRouter(fleet.NewManager(cfg), cfg, unreachablePool(t),
				nil, nil, nil, nil)
			buf := captureAccessLog(t)
			// The channel lookup cannot reach the database: 503.
			code := accessServe(h, http.MethodPost, "/api/v1/chatops/slack/7?sig=q-secret",
				`{"text":"body-secret"}`, "")
			if code != http.StatusServiceUnavailable {
				t.Fatalf("callback = %d, want 503", code)
			}
			lines := accessLines(buf)
			if len(lines) != tc.wantLines {
				t.Fatalf("access lines = %v, want %d", lines, tc.wantLines)
			}
			if tc.wantLines == 0 {
				return
			}
			line := lines[0]
			for _, want := range []string{"method=POST", "status=503",
				`pattern="POST /api/v1/chatops/slack/{channel}"`} {
				if !strings.Contains(line, want) {
					t.Errorf("access line %q lacks %q", line, want)
				}
			}
			if strings.Contains(line, "user_id") || strings.Contains(buf.String(), "secret") {
				t.Errorf("access line carries a user or a secret: %q", buf.String())
			}
		})
	}
}
