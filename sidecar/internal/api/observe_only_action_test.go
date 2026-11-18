package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// fleetpol-01d: a manual action on an observe-only member is refused with
// 409 "observe-only member"; an unknown database is still 404.
// No concurrent access tests: the handler holds no state of its own.
func TestFleetManualExecute_RefusesAnObserveOnlyMember(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.SetObserveOnly([]string{"obs-1"})
	h := fleetManualExecuteHandler(mgr)
	cases := []struct {
		db     string
		status int
		want   string
	}{
		{"obs-1", http.StatusConflict, "observe-only member"},
		{"nope", http.StatusNotFound, "database action executor not found"},
	}
	for _, c := range cases {
		body := `{"database":"` + c.db + `","finding_id":7,"sql":"VACUUM t"}`
		req := withUser(httptest.NewRequest(http.MethodPost, "/api/v1/actions/execute",
			strings.NewReader(body)), testAdminUser())
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want %d %q", c.db, rec.Code, rec.Body.String(), c.status,
				c.want)
		}
	}
}
