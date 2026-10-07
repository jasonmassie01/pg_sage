package config

import (
	"strings"
	"testing"
)

// The quickstart must not ask readers to turn on what is already on: MCP
// answers over HTTP by default and only needs a token.
func TestQuickstartDoesNotAskToEnableDefaultOnMCP(t *testing.T) {
	doc := readQuickstart(t)
	if !DefaultMCPEnabled || DefaultMCPTransport != "http" {
		t.Fatalf("MCP defaults changed (enabled=%v transport=%q): revisit the quickstart "+
			"MCP row", DefaultMCPEnabled, DefaultMCPTransport)
	}
	for _, line := range strings.Split(doc, "\n") {
		if !strings.Contains(line, "MCP for coding agents") {
			continue
		}
		if strings.Contains(line, "`mcp.enabled`") || strings.Contains(line, "`mcp.transport`") {
			t.Fatalf("the MCP row asks to turn on a default: %s", line)
		}
		if !strings.Contains(line, `**"MCP tokens"**`) || !strings.Contains(line, "mcp.md") {
			t.Fatalf("the MCP row must point at MCP tokens and the MCP guide: %s", line)
		}
		return
	}
	t.Fatal("docs/quickstart.md has no \"MCP for coding agents\" row")
}
