//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// clearForcedPasswordChange finishes the apiauth-03 forced change for the
// smoke admin. The generated bootstrap password is flagged
// must_change_password, and every other API call answers 403 until it is
// replaced; the suite changes it to a throwaway and back to password, so
// SAGE_E2E_ADMIN_PASSWORD stays valid for the next run. The calling
// session survives both changes.
func clearForcedPasswordChange(t *testing.T, env *testEnv, password string, body io.Reader) {
	t.Helper()
	var login struct {
		MustChange bool `json:"must_change_password"`
	}
	if err := json.NewDecoder(body).Decode(&login); err != nil {
		t.Fatalf("decoding the login response: %v", err)
	}
	if !login.MustChange {
		return
	}
	interim := "e2e-interim-" + strings.Repeat("x", 12)
	changeAdminPassword(t, env, password, interim)
	changeAdminPassword(t, env, interim, password)
	t.Log("replaced the generated admin password (must_change_password)")
}

func changeAdminPassword(t *testing.T, env *testEnv, current, next string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{
		"current_password": current, "new_password": next,
	})
	req, err := http.NewRequest(http.MethodPut, env.apiBase+"/api/v1/auth/password",
		strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("building the password change: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	status, resp := doRequest(t, env, req)
	if status != http.StatusOK {
		t.Fatalf("forced password change = %d %.300s, want 200", status, resp)
	}
}
