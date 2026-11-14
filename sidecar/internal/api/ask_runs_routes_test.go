package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
)

// mcp-07b: the run routes keep the default request deadline (only the
// synchronous question gets the ask setting) and map the run errors to
// distinguishable responses. No concurrent access tests: pure functions.

func TestAskRunsRoutes_AreNotTheSynchronousQuestion(t *testing.T) {
	for _, path := range []string{"/api/v1/databases/db1/ask/runs",
		"/api/v1/databases/db1/ask/runs/6f1c1a52-6d1f-4d55-9a43-6a0f3c1d2e3f/cancel"} {
		if isAskQuestion(httptest.NewRequest("POST", path, nil)) {
			t.Errorf("POST %s got the long ask deadline", path)
		}
	}
}

func TestAskError_RunErrors(t *testing.T) {
	cases := []struct {
		err        error
		status     int
		code       string
		retryAfter bool
	}{
		{fmt.Errorf("%w: 4 runs", ask.ErrBusy), http.StatusTooManyRequests, "ask_busy", true},
		{fmt.Errorf("%w: running", ask.ErrConflict), http.StatusConflict, "ask_conflict",
			false},
		{fmt.Errorf("%w: run x", ask.ErrNotFound), http.StatusNotFound, "not_found", false},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		askError(w, c.err)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.code) ||
			(w.Header().Get("Retry-After") != "") != c.retryAfter {
			t.Errorf("%v: %d %s (Retry-After %q)", c.err, w.Code, w.Body.String(),
				w.Header().Get("Retry-After"))
		}
	}
}
