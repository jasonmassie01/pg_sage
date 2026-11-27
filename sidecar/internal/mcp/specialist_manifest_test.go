package mcp

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// deploy/specialist (mcp-02) packages pg_sage for generalist SRE agents:
// an MCP server manifest whose tool list is generated from this registry
// and an agent instruction file. The tests fail when either drifts;
// regenerate the manifest with
//
//	go test ./internal/mcp -run TestSpecialistManifestMatchesTheRegistry -update-manifest

var updateManifest = flag.Bool("update-manifest", false,
	"rewrite deploy/specialist/server.json from the tool registry")

const (
	manifestPath = "../../../deploy/specialist/server.json"
	skillPath    = "../../../deploy/specialist/pg_sage-skill.md"
	chartPath    = "../../../deploy/helm/pg-sage/Chart.yaml"
	publisherKey = "io.modelcontextprotocol.registry/publisher-provided"
)

// manifestTool is one registry tool as the manifest lists it.
type manifestTool struct {
	Name     string `json:"name"`
	Scope    string `json:"scope"`
	ReadOnly bool   `json:"read_only"`
}

func chartAppVersion(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(chartPath))
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^appVersion:\s*"?([0-9][^"\s]*)"?`).FindSubmatch(raw)
	require.True(t, m != nil, "Chart.yaml has no appVersion")
	return string(m[1])
}

// manifestTools is the registry as the manifest lists it, in order.
func manifestTools(tools []Tool) []manifestTool {
	out := make([]manifestTool, 0, len(tools))
	for _, tool := range tools {
		scope, _ := RequiredScope(tool.Name, json.RawMessage(`{}`))
		out = append(out, manifestTool{Name: tool.Name, Scope: string(scope),
			ReadOnly: tool.Annotations != nil && tool.Annotations.ReadOnlyHint})
	}
	return out
}

// expectedManifest is the whole server.json the registry generates.
func expectedManifest(t *testing.T) []byte {
	t.Helper()
	manifest := map[string]any{
		"$schema": "https://static.modelcontextprotocol.io/schemas/2025-09-29/" +
			"server.schema.json",
		"name":  "io.github.jasonmassie01/pg_sage",
		"title": "pg_sage Postgres specialist",
		"description": "Agentic DBA for PostgreSQL: evidence-cited investigations, " +
			"held remediations a person approves.",
		"version":    chartAppVersion(t),
		"websiteUrl": "https://github.com/jasonmassie01/pg_sage",
		"remotes": []any{map[string]any{"type": "streamable-http",
			"url": "https://{sage_host}/api/v1/mcp",
			"variables": map[string]any{"sage_host": map[string]any{"isRequired": true,
				"description": "pg_sage API host and port, such as sage.example.com:8080"}},
			"headers": []any{map[string]any{"name": "Authorization", "isRequired": true,
				"isSecret": true, "value": "Bearer {pg_sage_mcp_token}",
				"description": "A pg_sage MCP token (pgs_mcp_...): read to investigate, " +
					"propose to request remediations. Never approve."}}}},
		"_meta": map[string]any{publisherKey: map[string]any{
			"contract_version": "pg_sage.specialist.v1",
			"contract_schema":  "/api/v1/specialist/openapi.json",
			"instructions":     "pg_sage-skill.md",
			"tools":            manifestTools(NewServer(&recordingBackend{}).Tools())}},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(manifest))
	return buf.Bytes()
}

func TestSpecialistManifestMatchesTheRegistry(t *testing.T) {
	want := expectedManifest(t)
	if *updateManifest {
		require.NoError(t, os.WriteFile(filepath.Clean(manifestPath), want, 0o600))
		return
	}
	got, err := os.ReadFile(filepath.Clean(manifestPath))
	require.NoError(t, err)
	require.Equal(t, string(want), strings.ReplaceAll(string(got), "\r\n", "\n"),
		"deploy/specialist/server.json is stale; regenerate it with -update-manifest")
}

func TestSpecialistManifestListsEveryToolWithItsScope(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(manifestPath))
	require.NoError(t, err)
	var doc struct {
		Remotes []struct {
			Type, URL string
			Headers   []struct {
				Name     string
				IsSecret bool `json:"isSecret"`
			}
		}
		Meta map[string]struct {
			Tools []manifestTool `json:"tools"`
		} `json:"_meta"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Equal(t, 1, len(doc.Remotes), "one remote")
	r := doc.Remotes[0]
	require.Equal(t, "streamable-http", r.Type)
	require.True(t, strings.HasSuffix(r.URL, "/api/v1/mcp"), "endpoint %s", r.URL)
	require.True(t, len(r.Headers) == 1 && r.Headers[0].Name == "Authorization" &&
		r.Headers[0].IsSecret, "bearer auth header %+v", r.Headers)
	listed := doc.Meta[publisherKey].Tools
	tools := NewServer(&recordingBackend{}).Tools()
	require.Equal(t, len(tools), len(listed), "every registry tool is listed once")
	byName := map[string]manifestTool{}
	for _, tool := range listed {
		byName[tool.Name] = tool
	}
	for _, tool := range tools {
		m, ok := byName[tool.Name]
		require.True(t, ok, "%s is missing from the manifest", tool.Name)
		scope, _ := RequiredScope(tool.Name, json.RawMessage(`{}`))
		require.Equal(t, string(scope), m.Scope, tool.Name)
		require.True(t, m.Scope == "read" || m.Scope == "propose" || m.Scope == "approve",
			"%s has scope %q", tool.Name, m.Scope)
	}
	require.Equal(t, "read", byName["specialist_open_investigation"].Scope)
	require.Equal(t, "propose", byName["specialist_request_remediation"].Scope)
}

var skillToolName = regexp.MustCompile(`\b(specialist_[a-z_]+|get_request_status|` +
	`wait_for_decision|list_databases)\b`)

func TestSpecialistSkillNamesOnlyRealToolsInTheSessionOrder(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(skillPath))
	require.NoError(t, err)
	text := string(raw)
	known := map[string]bool{}
	for _, tool := range NewServer(&recordingBackend{}).Tools() {
		known[tool.Name] = true
	}
	for _, name := range skillToolName.FindAllString(text, -1) {
		require.True(t, known[name], "the skill names %s, which the server does not serve",
			name)
	}
	last := -1
	for _, step := range []string{"specialist_open_investigation",
		"specialist_investigation_status", "specialist_investigation_result",
		"specialist_request_remediation", "wait_for_decision", "get_request_status"} {
		at := strings.Index(text, step)
		require.True(t, at > last, "%s must come after the previous step", step)
		last = at
	}
	for _, never := range []string{"never approves", "never runs SQL"} {
		require.Contains(t, text, never)
	}
}
