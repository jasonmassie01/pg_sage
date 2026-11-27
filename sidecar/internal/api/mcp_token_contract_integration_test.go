//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Contract (agentgov-01): web/src/pages/mcptokens/testdata/default-create.json
// is the body the MCP Tokens page posts from its default form, written by
// the page's Vitest contract test. Posting it to the real handler on real
// PostgreSQL must create a token, so a mocked 201 in the page tests cannot
// hide drift between the page and the server again. No concurrency test:
// the server path is unchanged and covered by the route tests.

var defaultCreateFixture = filepath.Join("..", "..", "web", "src", "pages", "mcptokens",
	"testdata", "default-create.json")

// loadDefaultCreate reads the page's default body, keeping its fields as
// the page sent them (DisallowUnknownFields on the server checks the rest).
func loadDefaultCreate(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(defaultCreateFixture)
	require.NoError(t, err, "run the web contract test to write %s", defaultCreateFixture)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body), string(raw))
	return body
}

// postFixture posts body with a unique name and a real owner, the two values
// the page fills from the admin's input.
func postFixture(t *testing.T, h http.Handler, body map[string]any,
	owner int) (int, string, string) {
	t.Helper()
	name := fmt.Sprintf("contract-%d", time.Now().UnixNano())
	body["name"] = name
	if _, ok := body["owner_user_id"]; ok {
		body["owner_user_id"] = owner
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	code, resp := tokenCall(t, h, http.MethodPost, tokenRoutesPath, string(raw))
	return code, resp, name
}

func TestContractDefaultCreateFormIsAccepted(t *testing.T) {
	body := loadDefaultCreate(t)
	require.Equal(t, "operator", body["kind"],
		"the MCP Tokens page must post operator tokens; agent tokens are minted "+
			"per agent at POST /api/v1/agents/{id}/tokens")
	_, hasOwner := body["owner_user_id"]
	require.True(t, hasOwner, "an operator token needs owner_user_id: %v", body)

	pool := surfacePool(t)
	h := tokenRouter(t, pool, tokenRouteUser(t, pool, auth.RoleAdmin))
	owner := tokenRouteUser(t, pool, auth.RoleOperator)
	code, resp, name := postFixture(t, h, body, owner.ID)
	require.Equal(t, http.StatusCreated, code, resp)

	created := decodeObject(t, resp)
	require.Equal(t, name, created["name"])
	require.Equal(t, "operator", created["kind"])
	require.Equal(t, float64(owner.ID), created["owner_user_id"])
	require.Equal(t, body["scopes"], created["scopes"])
	require.Equal(t, body["databases"], created["databases"])
	require.Equal(t, 1, countTokensNamed(t, pool, name))
	entry, _ := listedToken(t, h, created["id"].(string))
	require.Equal(t, float64(owner.ID), entry["owner_user_id"])
}

// TestContractDefaultCreateAsAgentIsRefused pins the server rule the page
// relies on: the same body with kind "agent" is refused here, so a fixture
// that drifts back to agent tokens fails the contract above.
func TestContractDefaultCreateAsAgentIsRefused(t *testing.T) {
	body := loadDefaultCreate(t)
	body["kind"] = "agent"
	delete(body, "owner_user_id")

	pool := surfacePool(t)
	h := tokenRouter(t, pool, tokenRouteUser(t, pool, auth.RoleAdmin))
	code, resp, name := postFixture(t, h, body, 0)
	require.Equal(t, http.StatusBadRequest, code, resp)
	require.Contains(t, resp, "/api/v1/agents/{id}/tokens")
	require.Equal(t, 0, countTokensNamed(t, pool, name))
}

// TestContractDefaultCreateWithoutOwnerIsRefused: the page disables submit
// without an owner; the server refuses such a body too.
func TestContractDefaultCreateWithoutOwnerIsRefused(t *testing.T) {
	body := loadDefaultCreate(t)
	delete(body, "owner_user_id")

	pool := surfacePool(t)
	h := tokenRouter(t, pool, tokenRouteUser(t, pool, auth.RoleAdmin))
	code, resp, name := postFixture(t, h, body, 0)
	require.Equal(t, http.StatusBadRequest, code, resp)
	require.Contains(t, resp, "owner")
	require.Equal(t, 0, countTokensNamed(t, pool, name))
}
