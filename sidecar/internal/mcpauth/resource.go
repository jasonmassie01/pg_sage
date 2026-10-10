package mcpauth

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// MetadataPrefix is RFC 9728's well-known path prefix.
const MetadataPrefix = "/.well-known/oauth-protected-resource"

const databasesSegment = "/databases/"

// ResourceFor is the resource identifier of one database under resource.
func ResourceFor(resource, database string) string {
	return strings.TrimRight(resource, "/") + databasesSegment + url.PathEscape(database)
}

// ResourceForPath maps a request's escaped path to the resource it
// addresses and that resource's database ("" for the server-wide one).
// Unknown databases and other paths are not resources.
func (v *Validator) ResourceForPath(escapedPath string) (string, string, bool) {
	base := v.base.EscapedPath()
	if escapedPath == base {
		return v.resource, "", true
	}
	rest, ok := strings.CutPrefix(escapedPath, base+databasesSegment)
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", "", false
	}
	db, err := url.PathUnescape(rest)
	if err != nil || !contains(v.databases(), db) {
		return "", "", false
	}
	return ResourceFor(v.resource, db), db, true
}

// metadataURL is the RFC 9728 metadata location of resource: the
// well-known prefix inserted between the host and the resource's path.
func (v *Validator) metadataURL(resource string) string {
	u, err := url.Parse(resource)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + MetadataPrefix + u.EscapedPath()
}

// Challenge is the WWW-Authenticate value of a refused request for
// resource; code is an RFC 6750 error code or "" when no token was sent.
func (v *Validator) Challenge(resource, code string) string {
	out := `Bearer resource_metadata="` + v.metadataURL(resource) + `"`
	if code != "" {
		out += `, error="` + code + `"`
	}
	return out
}

// protectedResource is the RFC 9728 metadata document.
type protectedResource struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
}

// MetadataHandler serves the metadata of the server-wide resource and of
// every monitored database's resource. It needs no authentication.
func (v *Validator) MetadataHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, ok := strings.CutPrefix(r.URL.EscapedPath(), MetadataPrefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		resource, db, ok := v.ResourceForPath(path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		name := "pg_sage MCP"
		if db != "" {
			name += " (" + db + ")"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=300")
		_ = json.NewEncoder(w).Encode(protectedResource{Resource: resource,
			AuthorizationServers: v.order, ResourceName: name,
			ScopesSupported:        []string{ScopeRead, ScopePropose},
			BearerMethodsSupported: []string{"header"}})
	})
}
