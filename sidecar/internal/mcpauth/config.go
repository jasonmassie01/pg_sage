// Package mcpauth makes the MCP endpoint an OAuth 2.1 resource server (E2,
// CG-06, spec §6.4): it validates bearer access tokens from an allowlist of
// issuers (signature against the issuer's JWKS, issuer, expiry, audience),
// maps the token's identity to a pg_sage principal, and publishes the
// protected-resource metadata of RFC 9728 that the MCP authorization spec
// requires.
//
// Audience binds a token to this pg_sage and to one database. Every
// monitored database is its own protected resource,
// <resource>/databases/<name>, served at that path: a token issued for one
// database's resource is refused at another's. The server-wide resource
// (the MCP endpoint itself) accepts tokens whose audience names it, and
// reaches only the databases whose resources the same audience also names.
package mcpauth

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Config is the resource server's configuration (mcp.oauth).
type Config struct {
	// Resource is the canonical URI of the MCP endpoint, for example
	// https://sage.example.com/api/v1/mcp.
	Resource string
	// Issuers is the allowlist of authorization servers.
	Issuers []Issuer
	// TaskClaim names the runtime-asserted task claim ("" = none).
	TaskClaim string
}

// Issuer is one allowed authorization server.
type Issuer struct {
	Issuer string
	// JWKSURI is the issuer's key set; empty discovers it from the
	// issuer's RFC 8414 (or OpenID) metadata.
	JWKSURI string
	// SigningAlgs restricts the accepted algorithms (default: the
	// asymmetric RS, PS and ES families and EdDSA).
	SigningAlgs []string
}

// defaultAlgs are asymmetric only: a symmetric (HS*) key would have to be
// shared with pg_sage, and "none" is never a signature.
var defaultAlgs = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512",
	"ES256", "ES384", "ES512", "EdDSA"}

func (c Config) validate() error {
	if err := checkURL("mcp.oauth.resource", c.Resource); err != nil {
		return err
	}
	if len(c.Issuers) == 0 {
		return fmt.Errorf("mcp.oauth.issuers: at least one issuer is required")
	}
	seen := map[string]bool{}
	for _, iss := range c.Issuers {
		if err := checkURL("mcp.oauth.issuers[].issuer", iss.Issuer); err != nil {
			return err
		}
		if iss.JWKSURI != "" {
			if err := checkURL("mcp.oauth.issuers[].jwks_uri", iss.JWKSURI); err != nil {
				return err
			}
		}
		if seen[iss.Issuer] {
			return fmt.Errorf("mcp.oauth.issuers: %s is listed twice", iss.Issuer)
		}
		seen[iss.Issuer] = true
		for _, alg := range iss.SigningAlgs {
			if !isAsymmetric(alg) {
				return fmt.Errorf("mcp.oauth.issuers[%s].signing_algs: %q is not an "+
					"asymmetric JWS algorithm", iss.Issuer, alg)
			}
		}
	}
	if len(c.TaskClaim) > 64 {
		return fmt.Errorf("mcp.oauth.task_claim is longer than 64 characters")
	}
	return nil
}

func isAsymmetric(alg string) bool {
	for _, a := range defaultAlgs {
		if a == alg {
			return true
		}
	}
	return false
}

// checkURL accepts https URLs, and http only on a loopback host (local
// development); never a fragment or query.
func checkURL(key, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: %q is not an absolute URL", key, raw)
	}
	if u.Fragment != "" || u.RawQuery != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("%s: %q must not carry a query or fragment", key, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%s: %q must use https (http is allowed only on loopback)",
			key, raw)
	}
	return fmt.Errorf("%s: %q must use https", key, raw)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
