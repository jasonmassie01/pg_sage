package config

import "testing"

// MCP v2: the API server always runs, so MCP is served over HTTP (behind
// its authentication) unless the operator chooses stdio.
func TestMCPTransportDefaultsToHTTP(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MCP.Transport != "http" || !cfg.MCP.Enabled {
		t.Fatalf("MCP default = %#v, want enabled http", cfg.MCP)
	}
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("default MCP config invalid: %v", err)
	}
}

func TestMCPTransportStdioStaysAvailable(t *testing.T) {
	cfg := DefaultConfig()
	if err := loadYAML(writeYAMLFile(t, "mcp:\n  transport: stdio\n"), cfg); err != nil {
		t.Fatalf("loadYAML: %v", err)
	}
	if cfg.MCP.Transport != "stdio" {
		t.Fatalf("transport = %q, want stdio", cfg.MCP.Transport)
	}
	if err := cfg.validateAgentNative(); err != nil {
		t.Fatalf("stdio config invalid: %v", err)
	}
	t.Setenv("SAGE_MCP_TRANSPORT", "stdio")
	env := DefaultConfig()
	overlayEnv(env)
	if env.MCP.Transport != "stdio" {
		t.Fatalf("env transport = %q, want stdio", env.MCP.Transport)
	}
}

func TestMCPTransportRejectsUnknownValues(t *testing.T) {
	for _, transport := range []string{"", "HTTP", "sse", "websocket"} {
		cfg := DefaultConfig()
		cfg.MCP.Transport = transport
		if err := cfg.validateAgentNative(); err == nil {
			t.Fatalf("transport %q accepted", transport)
		}
	}
}
