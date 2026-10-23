package config

import "fmt"

// MCPOAuthConfig makes MCP over HTTP an OAuth 2.1 resource server (E2,
// CG-06): bearer access tokens from the listed issuers are accepted beside
// pg_sage's own MCP tokens. Every key widens who may call MCP, so all are
// safety-critical.
type MCPOAuthConfig struct {
	Enabled   bool             `yaml:"enabled" doc:"Accept OAuth 2.1 access tokens (JWTs) from the listed issuers on the MCP endpoint, beside pg_sage's own MCP tokens. Default: false."`
	Resource  string           `yaml:"resource" doc:"Canonical URL of the MCP endpoint, e.g. https://sage.example.com/api/v1/mcp. Tokens must name it or <resource>/databases/<name> as audience. Required when enabled."`
	Issuers   []MCPOAuthIssuer `yaml:"issuers" doc:"Allowed authorization servers: [{issuer, jwks_uri, signing_algs}]. jwks_uri empty discovers it from the issuer's metadata. Required when enabled."`
	TaskClaim string           `yaml:"task_claim" doc:"Access-token claim that carries the runtime-asserted task id (per-task taint). Empty ignores tasks. Default: task_id."`
}

// MCPOAuthIssuer is one allowed authorization server.
type MCPOAuthIssuer struct {
	Issuer      string   `yaml:"issuer"`
	JWKSURI     string   `yaml:"jwks_uri"`
	SigningAlgs []string `yaml:"signing_algs"`
}

// DefaultMCPOAuthTaskClaim is the default task claim name.
const DefaultMCPOAuthTaskClaim = "task_id"

func defaultMCPOAuthConfig() MCPOAuthConfig {
	return MCPOAuthConfig{TaskClaim: DefaultMCPOAuthTaskClaim}
}

// validate checks the shape when enabled; mcpauth.New checks URLs and
// algorithms in full when the server starts.
func (o MCPOAuthConfig) validate(mcpTransport string) error {
	if !o.Enabled {
		return nil
	}
	if mcpTransport != "http" {
		return fmt.Errorf("mcp.oauth.enabled needs mcp.transport: http")
	}
	if o.Resource == "" {
		return fmt.Errorf("mcp.oauth.resource is required when mcp.oauth is enabled")
	}
	if len(o.Issuers) == 0 {
		return fmt.Errorf("mcp.oauth.issuers needs at least one issuer when enabled")
	}
	for i, iss := range o.Issuers {
		if iss.Issuer == "" {
			return fmt.Errorf("mcp.oauth.issuers[%d].issuer is required", i)
		}
	}
	return nil
}
