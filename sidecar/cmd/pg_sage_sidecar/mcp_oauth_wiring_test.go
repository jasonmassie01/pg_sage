package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcpauth"
)

func oauthConfig(resource string) *config.Config {
	c := config.DefaultConfig()
	c.MCP.Enabled, c.MCP.Transport = true, "http"
	c.MCP.OAuth = config.MCPOAuthConfig{Enabled: true, Resource: resource,
		TaskClaim: "task_id", Issuers: []config.MCPOAuthIssuer{{
			Issuer: "https://idp.example.com", SigningAlgs: []string{"ES256"}}}}
	return c
}

// The validator exists only when MCP runs over HTTP with oauth enabled and
// a configuration mcpauth accepts; otherwise only pg_sage tokens work.
func TestMCPOAuthValidatorWiring(t *testing.T) {
	good := oauthConfig("https://sage.example.com/api/v1/mcp")
	v := mcpOAuthValidator(good, nil, nil)
	if v == nil || v.Resource() != "https://sage.example.com/api/v1/mcp" {
		t.Fatalf("configured validator = %v", v)
	}
	if _, _, ok := v.ResourceForPath("/api/v1/mcp/databases/x"); ok {
		t.Fatalf("without a fleet no database resource may exist")
	}
	off := oauthConfig("https://sage.example.com/api/v1/mcp")
	off.MCP.OAuth.Enabled = false
	stdio := oauthConfig("https://sage.example.com/api/v1/mcp")
	stdio.MCP.Transport = "stdio"
	plainHTTP := oauthConfig("http://sage.example.com/api/v1/mcp")
	for name, c := range map[string]*config.Config{"disabled": off, "stdio": stdio,
		"non-loopback http": plainHTTP, "nil": nil} {
		if got := mcpOAuthValidator(c, nil, nil); got != nil {
			t.Fatalf("%s: validator built", name)
		}
	}
}

// The validator's resolver is the bindings table on the control pool;
// without a control pool every identity is unbound.
func TestMCPOAuthResolverWithoutControlPool(t *testing.T) {
	r := mcpauth.NewDBResolver(nil)
	if _, err := r.PrincipalForSubject(t.Context(), "https://idp", "s"); err == nil {
		t.Fatalf("a nil control pool resolved an identity")
	}
}
