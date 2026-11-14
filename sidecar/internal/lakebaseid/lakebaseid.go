// Package lakebaseid is the one rule for Lakebase endpoint resource names and
// for the SAGE_LAKEBASE_ENDPOINT(S) host mapping. OAuth, history telemetry
// config and telemetry scope all call it, so a name one accepts the others
// accept too. It imports nothing internal and may be used by any package.
package lakebaseid

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Pattern is one project, branch or endpoint ID: 1-63 lowercase letters,
// digits or hyphens, not starting with a hyphen. Lakebase creates IDs that
// start with a letter; a leading digit stays accepted because OAuth always
// accepted it and the endpoint is verified against live metadata anyway.
// deploy/databricks-apps/configure.py copies this text as SEGMENT.
const Pattern = `[a-z0-9][a-z0-9-]{0,62}`

// RehearsalBranchPrefix starts the id of every branch pg_sage creates for a
// rehearsal (sage-rh-<8 hex>). The clone provider creates and destroys only
// such branches, and fleet discovery skips them.
const RehearsalBranchPrefix = "sage-rh-"

var resource = regexp.MustCompile(`^projects/(` + Pattern + `)/branches/(` + Pattern +
	`)/endpoints/(` + Pattern + `)$`)

// errForm names the accepted shape; callers keep this text in their errors.
var errForm = errors.New(
	"lakebase endpoint must be projects/<id>/branches/<id>/endpoints/<id>")

// Valid reports whether name is a well-formed endpoint resource.
func Valid(name string) bool {
	return resource.MatchString(name)
}

// Parse splits a well-formed endpoint resource into its three IDs.
func Parse(name string) (project, branch, endpoint string, err error) {
	m := resource.FindStringSubmatch(name)
	if m == nil {
		return "", "", "", errForm
	}
	return m[1], m[2], m[3], nil
}

// EndpointForHost returns the endpoint resource SAGE_LAKEBASE_ENDPOINT or
// SAGE_LAKEBASE_ENDPOINTS (a JSON object mapping hostnames to resources)
// configures for host, or "" when host is ordinary PostgreSQL. Setting both
// variables, or any malformed resource in either, is an error.
func EndpointForHost(getenv func(string) string, host string) (string, error) {
	if getenv == nil {
		return "", fmt.Errorf("lakebase endpoint lookup needs an environment reader")
	}
	endpoint := getenv("SAGE_LAKEBASE_ENDPOINT")
	if raw := getenv("SAGE_LAKEBASE_ENDPOINTS"); raw != "" {
		if endpoint != "" {
			return "", fmt.Errorf(
				"set only one of SAGE_LAKEBASE_ENDPOINT and SAGE_LAKEBASE_ENDPOINTS")
		}
		mapped, err := mappedEndpoint(raw, host)
		if err != nil {
			return "", err
		}
		endpoint = mapped
	}
	if endpoint != "" && !Valid(endpoint) {
		return "", errForm
	}
	return endpoint, nil
}

func mappedEndpoint(raw, host string) (string, error) {
	var endpoints map[string]string
	if err := json.Unmarshal([]byte(raw), &endpoints); err != nil {
		return "", fmt.Errorf("SAGE_LAKEBASE_ENDPOINTS must be a JSON object " +
			"mapping hostnames to endpoint resource paths")
	}
	endpoint := ""
	for mapped, name := range endpoints {
		if !Valid(name) {
			return "", fmt.Errorf(
				"invalid Lakebase endpoint resource path in SAGE_LAKEBASE_ENDPOINTS")
		}
		if sameHost(host, mapped) {
			endpoint = name
		}
	}
	return endpoint, nil
}

// sameHost compares DNS names case-insensitively, ignoring a trailing dot.
func sameHost(a, b string) bool {
	return b != "" && strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}
