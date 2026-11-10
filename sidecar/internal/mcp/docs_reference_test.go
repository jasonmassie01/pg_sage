package mcp

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// docs/mcp.md carries a tool reference generated from the tool schemas.
// This test fails when the two drift; regenerate with
//
//	go test ./internal/mcp -run TestToolReferenceDocsMatchSchemas -update-docs

var updateDocs = flag.Bool("update-docs", false, "rewrite the generated MCP tool reference")

const mcpDocsPath = "../../../docs/mcp.md"

func TestToolReferenceDocsMatchSchemas(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(mcpDocsPath))
	require.NoError(t, err)
	doc := string(raw)
	begin := strings.Index(doc, ToolReferenceBegin)
	end := strings.Index(doc, ToolReferenceEnd)
	require.True(t, begin >= 0 && end > begin, "generated markers missing in docs/mcp.md")
	generated := ToolReferenceMarkdown(NewServer(&recordingBackend{}).Tools())
	current := doc[begin+len(ToolReferenceBegin) : end]
	if *updateDocs {
		updated := doc[:begin+len(ToolReferenceBegin)] + generated + doc[end:]
		require.NoError(t, os.WriteFile(filepath.Clean(mcpDocsPath), []byte(updated), 0o600))
		return
	}
	require.Equal(t, generated, current, "docs/mcp.md tool reference is stale")
}

// Boundary (mcp-11): the begin marker ends in a newline, so a CRLF
// checkout of docs/mcp.md hides it and the drift test reports "markers
// missing". .gitattributes pins the file to LF; this names that cause.
func TestToolReferenceMarkersFoundInFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(mcpDocsPath))
	require.NoError(t, err)
	require.False(t, strings.Contains(string(raw), "\r"),
		"docs/mcp.md has CRLF line endings; it must be LF (.gitattributes eol=lf), "+
			"run: git checkout -- docs/mcp.md")
	doc := string(raw)
	require.Equal(t, 1, strings.Count(doc, ToolReferenceBegin), "begin marker count")
	require.Equal(t, 1, strings.Count(doc, ToolReferenceEnd), "end marker count")
	require.True(t, strings.Index(doc, ToolReferenceBegin) < strings.Index(doc, ToolReferenceEnd),
		"begin marker must precede end marker")
}

// No concurrent access test: the doc file is read-only test input.

func TestToolReferenceCoversEveryToolAndScope(t *testing.T) {
	tools := NewServer(&recordingBackend{}).Tools()
	text := ToolReferenceMarkdown(tools)
	for _, tool := range tools {
		require.Contains(t, text, "### `"+tool.Name+"`")
		scope, _ := RequiredScope(tool.Name, nil)
		section := text[strings.Index(text, "### `"+tool.Name+"`"):]
		if next := strings.Index(section[4:], "\n### "); next > 0 {
			section = section[:next+4]
		}
		require.Contains(t, section, "Scope: `"+string(scope)+"`", tool.Name)
		properties := objectMap(t, decodeSchema(t, tool.InputSchema)["properties"])
		for name := range properties {
			require.Contains(t, section, "`"+name+"`", "%s.%s", tool.Name, name)
		}
	}
	require.Equal(t, text, ToolReferenceMarkdown(tools), "generation is deterministic")
}

func TestDocsShowTheClaudeCodeSetup(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(mcpDocsPath))
	require.NoError(t, err)
	doc := string(raw)
	require.Contains(t, doc, "claude mcp add --transport http pg_sage ")
	require.Contains(t, doc, `--header "Authorization: Bearer `)
	require.Contains(t, doc, "/api/v1/mcp")
	require.NotContains(t, doc, "pgs_mcp_"+strings.Repeat("x", 10), "no real-looking token")
}
