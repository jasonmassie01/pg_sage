package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/broker"
)

// Spec §8.3: GET /api/v1/agents/{id}/activity (operator) →
// ?database&from&to&limit&cursor. The statements are attributed by role
// (G1-10); a dropped attribution is reported with the audit fallback.

const activityTestPrincipal = "agp_abcdefghijklmnopqrst"

type fakeActivity struct {
	pid, database string
	req           broker.ActivityRequest
	out           []broker.Activity
	err           error
	calls         int
}

func (f *fakeActivity) AgentActivity(_ context.Context, pid, database string,
	req broker.ActivityRequest) ([]broker.Activity, error) {
	f.calls++
	f.pid, f.database, f.req = pid, database, req
	return f.out, f.err
}

func activityMux(reader AgentActivityReader) *http.ServeMux {
	mux := http.NewServeMux()
	registerAgentActivityRoutes(mux, reader)
	return mux
}

func TestAgentActivityRoute(t *testing.T) {
	reader := &fakeActivity{out: []broker.Activity{{Database: "app",
		Attribution: broker.Attribution{Source: broker.SourceAudit, Dropped: true,
			Reason: broker.ReasonDeallocAdvanced}}}}
	mux := activityMux(reader)
	path := "/api/v1/agents/" + activityTestPrincipal + "/activity"
	if code, _ := doJSON(t, mux, nil, "GET", path, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	if code, _ := doJSON(t, mux, viewerUser(), "GET", path, nil); code != http.StatusForbidden {
		t.Fatalf("viewer: %d", code)
	}
	code, body := doJSON(t, mux, operatorUser(), "GET", path+
		"?database=app&from=2026-10-01T00:00:00Z&to=2026-10-02T00:00:00Z&limit=10&cursor=c1",
		nil)
	if code != http.StatusOK {
		t.Fatalf("operator: %d %v", code, body)
	}
	if reader.pid != activityTestPrincipal || reader.database != "app" ||
		reader.req.Limit != 10 || reader.req.Cursor != "c1" ||
		!reader.req.From.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) ||
		!reader.req.To.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("request = %s %s %+v", reader.pid, reader.database, reader.req)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", body)
	}
	attribution, _ := items[0].(map[string]any)["attribution"].(map[string]any)
	if attribution["dropped"] != true || attribution["source"] != broker.SourceAudit {
		t.Errorf("attribution = %v", attribution)
	}
}

func TestAgentActivityRouteDefaultsAndValidation(t *testing.T) {
	reader := &fakeActivity{}
	mux := activityMux(reader)
	base := "/api/v1/agents/" + activityTestPrincipal + "/activity"
	code, body := doJSON(t, mux, operatorUser(), "GET", base, nil)
	if code != http.StatusOK || reader.req.Limit != 50 || reader.database != "" {
		t.Fatalf("defaults: %d %v %+v", code, body, reader.req)
	}
	if got := reader.req.To.Sub(reader.req.From); got != 24*time.Hour {
		t.Errorf("default window = %v, want the last 24 hours", got)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 0 {
		t.Errorf("empty activity = %v, want items: []", body)
	}
	before := reader.calls
	for _, q := range []string{"?limit=0", "?limit=201", "?limit=x", "?from=yesterday",
		"?from=2026-10-02T00:00:00Z&to=2026-10-01T00:00:00Z", "?database=a%20b"} {
		if code, _ := doJSON(t, mux, operatorUser(), "GET", base+q, nil); code != 422 {
			t.Errorf("%s: %d, want 422", q, code)
		}
	}
	if code, _ := doJSON(t, mux, operatorUser(), "GET", "/api/v1/agents/bad-id/activity",
		nil); code != 422 {
		t.Errorf("malformed id: %d, want 422", code)
	}
	if reader.calls != before {
		t.Error("an invalid request reached the reader")
	}
}

func TestAgentActivityRouteErrors(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{agentguard.ErrNotFound, http.StatusNotFound},
		{broker.ErrUnknownDatabase, http.StatusNotFound},
		{broker.ErrInvalid, http.StatusUnprocessableEntity},
		{broker.ErrUnavailable, http.StatusServiceUnavailable},
		{errors.New("boom"), http.StatusInternalServerError},
	}
	path := "/api/v1/agents/" + activityTestPrincipal + "/activity"
	for _, c := range cases {
		mux := activityMux(&fakeActivity{err: c.err})
		if code, _ := doJSON(t, mux, operatorUser(), "GET", path, nil); code != c.code {
			t.Errorf("%v: %d, want %d", c.err, code, c.code)
		}
	}
	if code, _ := doJSON(t, activityMux(nil), operatorUser(), "GET", path, nil); code !=
		http.StatusServiceUnavailable {
		t.Errorf("no reader: %d, want 503", code)
	}
}
