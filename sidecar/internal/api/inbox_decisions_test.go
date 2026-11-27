package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/inbox"
)

// timeDecision and the access log's decided_from, without a database.

func TestTimeDecision_RecordsOnlySettledWaitingItems(t *testing.T) {
	waiting := time.Now().Add(-time.Hour)
	since := func(*http.Request) time.Time { return waiting }
	answer := func(code int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		})
	}
	call := func(h http.Handler, surface string) {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.Header.Set(inbox.SurfaceHeader, surface)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	before := inboxDecisions(inbox.KindPolicyException, "cli")
	call(timeDecision(inbox.KindPolicyException, since, answer(200)), "cli")
	call(timeDecision(inbox.KindPolicyException, since, answer(409)), "cli")
	call(timeDecision(inbox.KindPolicyException, since, answer(500)), "cli")
	none := func(*http.Request) time.Time { return time.Time{} }
	call(timeDecision(inbox.KindPolicyException, none, answer(200)), "cli")
	if got := inboxDecisions(inbox.KindPolicyException, "cli"); got != before+1 {
		t.Fatalf("decisions = %d, want %d: only the settled, waiting one", got, before+1)
	}
}

func TestAccessLog_NamesTheSurfaceADecisionCameFrom(t *testing.T) {
	buf := captureAccessLog(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for _, surface := range []string{"inbox", "Evil\"x", ""} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/facts/1/confirm", nil)
		if surface != "" {
			req.Header.Set(inbox.SurfaceHeader, surface)
		}
		accessLogMiddleware(ok).ServeHTTP(httptest.NewRecorder(), req)
	}
	lines := accessLines(buf)
	if len(lines) != 3 || !strings.Contains(lines[0], "decided_from=inbox") ||
		!strings.Contains(lines[1], "decided_from=api") ||
		strings.Contains(lines[2], "decided_from") {
		t.Fatalf("access lines %q", lines)
	}
}
