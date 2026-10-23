package mcpauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RFC 9728: the metadata lives at the well-known URI inserted before the
// resource's path, names the resource exactly and the allowed issuers.
func TestProtectedResourceMetadata(t *testing.T) {
	p := newIDP(t)
	v := newValidator(t, p, &mapResolver{}, "orders", "a b")
	h := v.MetadataHandler()
	cases := map[string]string{
		"/.well-known/oauth-protected-resource/api/v1/mcp": testResource,
		"/.well-known/oauth-protected-resource/api/v1/mcp/databases/orders": ResourceFor(
			testResource, "orders"),
		"/.well-known/oauth-protected-resource/api/v1/mcp/databases/a%20b": ResourceFor(
			testResource, "a b"),
	}
	for path, resource := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		var doc struct {
			Resource     string   `json:"resource"`
			AuthServers  []string `json:"authorization_servers"`
			Scopes       []string `json:"scopes_supported"`
			BearerMethod []string `json:"bearer_methods_supported"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if doc.Resource != resource || len(doc.AuthServers) != 1 ||
			doc.AuthServers[0] != p.URL() ||
			strings.Join(doc.Scopes, " ") != "pg_sage:read pg_sage:propose" ||
			strings.Join(doc.BearerMethod, ",") != "header" {
			t.Fatalf("%s: metadata = %+v", path, doc)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s: content type %q", path, ct)
		}
	}
}

// Unknown databases and paths outside the resource are not found.
func TestProtectedResourceMetadataNotFound(t *testing.T) {
	p := newIDP(t)
	h := newValidator(t, p, &mapResolver{}, "orders").MetadataHandler()
	for _, path := range []string{
		"/.well-known/oauth-protected-resource/api/v1/mcp/databases/billing",
		"/.well-known/oauth-protected-resource/api/v1/other",
		"/.well-known/oauth-protected-resource/api/v1/mcp/databases/orders/x",
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", path, w.Code)
		}
	}
}

// The challenge points clients at the metadata of the resource they hit.
func TestChallenge(t *testing.T) {
	p := newIDP(t)
	v := newValidator(t, p, &mapResolver{}, "orders")
	got := v.Challenge(ResourceFor(testResource, "orders"), "invalid_token")
	want := `Bearer resource_metadata="https://sage.example.com/.well-known/` +
		`oauth-protected-resource/api/v1/mcp/databases/orders", error="invalid_token"`
	if got != want {
		t.Fatalf("challenge = %s\nwant        %s", got, want)
	}
	if bare := v.Challenge(testResource, ""); strings.Contains(bare, "error=") {
		t.Fatalf("a challenge without an error code names one: %s", bare)
	}
}

// Resource identifiers: the database path segment is escaped, and the
// resource of a request path is recovered (or refused) exactly.
func TestResourceForPath(t *testing.T) {
	p := newIDP(t)
	v := newValidator(t, p, &mapResolver{}, "orders", "a/b")
	if got := ResourceFor(testResource, "a/b"); got != testResource+"/databases/a%2Fb" {
		t.Fatalf("ResourceFor escapes to %s", got)
	}
	cases := map[string]struct {
		resource, db string
		ok           bool
	}{
		"/api/v1/mcp":                     {testResource, "", true},
		"/api/v1/mcp/databases/orders":    {ResourceFor(testResource, "orders"), "orders", true},
		"/api/v1/mcp/databases/a%2Fb":     {ResourceFor(testResource, "a/b"), "a/b", true},
		"/api/v1/mcp/databases/billing":   {"", "", false},
		"/api/v1/mcp/databases/":          {"", "", false},
		"/api/v1/mcp/databases/orders/xx": {"", "", false},
	}
	for path, want := range cases {
		res, db, ok := v.ResourceForPath(path)
		if res != want.resource || db != want.db || ok != want.ok {
			t.Fatalf("%s = (%q, %q, %v), want %+v", path, res, db, ok, want)
		}
	}
}
