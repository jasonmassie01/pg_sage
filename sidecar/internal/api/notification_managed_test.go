package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/store"
)

// alerting-06b: a write to a channel or rule the YAML reconciler owns is
// 409 {"error":"managed_by_config"}; the other store errors keep their
// codes. The store side is pinned on real PostgreSQL in internal/store.
// No concurrent access tests: the mapping is a pure function of the error.

func TestNotificationWriteError_Codes(t *testing.T) {
	cases := []struct {
		err      error
		code     int
		wantBody string
	}{
		{fmt.Errorf("channel 3: %w", store.ErrManagedByConfig), http.StatusConflict,
			"managed_by_config"},
		{fmt.Errorf("%w: channel 3", store.ErrNotFound), http.StatusNotFound,
			"channel not found"},
		{fmt.Errorf("%w: name is reserved", store.ErrValidation), http.StatusBadRequest,
			"validation failed: name is reserved"},
		{errors.New("connection refused"), http.StatusInternalServerError, ""},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/api/v1/notifications/channels/3", nil)
		notificationWriteError(w, r, "update channel", "channel not found", tc.err)
		if w.Code != tc.code {
			t.Errorf("%v: status %d, want %d", tc.err, w.Code, tc.code)
			continue
		}
		var body map[string]string
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Errorf("%v: body is not JSON: %v", tc.err, err)
			continue
		}
		if tc.wantBody != "" && body["error"] != tc.wantBody {
			t.Errorf("%v: error %q, want %q", tc.err, body["error"], tc.wantBody)
		}
		if tc.code == http.StatusInternalServerError && body["error"] == tc.err.Error() {
			t.Errorf("internal error echoed the cause %q", body["error"])
		}
	}
}
