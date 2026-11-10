package config

import (
	"strings"
	"testing"
	"time"
)

// mcp.call_timeout_seconds bounds one stdio tools/call (mcp-04).

// Default value masking: with no config file the timeout is 90 s, not 0.
func TestMCPCallTimeoutDefault(t *testing.T) {
	c := DefaultConfig()
	if c.MCP.CallTimeoutSeconds != 90 {
		t.Fatalf("call_timeout_seconds = %d, want 90", c.MCP.CallTimeoutSeconds)
	}
	if got := c.MCP.CallTimeout(); got != 90*time.Second {
		t.Fatalf("CallTimeout() = %s, want 1m30s", got)
	}
	if err := c.validateAgentNative(); err != nil {
		t.Fatalf("default rejected: %v", err)
	}
}

// Nil/zero: a zero-value MCPConfig (built in code, never validated) still
// bounds calls at the default instead of not at all.
func TestMCPCallTimeoutZeroValueFallsBackToDefault(t *testing.T) {
	if got := (MCPConfig{}).CallTimeout(); got != 90*time.Second {
		t.Fatalf("zero CallTimeout() = %s, want 1m30s", got)
	}
	if got := (MCPConfig{CallTimeoutSeconds: 7}).CallTimeout(); got != 7*time.Second {
		t.Fatalf("CallTimeout() = %s, want 7s", got)
	}
}

// A partial mcp block keeps the default; a set value is read.
func TestMCPCallTimeoutYAML(t *testing.T) {
	path := wave5WriteYAML(t, "mcp:\n  transport: stdio\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.MCP.CallTimeoutSeconds != 90 {
		t.Fatalf("partial mcp block: call_timeout_seconds = %d, want 90",
			c.MCP.CallTimeoutSeconds)
	}
	path = wave5WriteYAML(t, "mcp:\n  call_timeout_seconds: 300\n")
	c = DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	if c.MCP.CallTimeoutSeconds != 300 || c.MCP.CallTimeout() != 5*time.Minute {
		t.Fatalf("loaded call_timeout_seconds = %d", c.MCP.CallTimeoutSeconds)
	}
}

// Boundaries: 5 s and 10 min are accepted; 4, 601, 0 and negatives are
// refused with an error that names the key and its range.
func TestMCPCallTimeoutValidation(t *testing.T) {
	for _, seconds := range []int{5, 6, 599, 600} {
		c := DefaultConfig()
		c.MCP.CallTimeoutSeconds = seconds
		if err := c.validateAgentNative(); err != nil {
			t.Errorf("call_timeout_seconds %d rejected: %v", seconds, err)
		}
	}
	for _, seconds := range []int{-1, 0, 4, 601, 3600} {
		c := DefaultConfig()
		c.MCP.CallTimeoutSeconds = seconds
		err := c.validateAgentNative()
		if err == nil {
			t.Errorf("call_timeout_seconds %d accepted", seconds)
			continue
		}
		if !strings.Contains(err.Error(), "mcp.call_timeout_seconds") ||
			!strings.Contains(err.Error(), "5-600") {
			t.Errorf("call_timeout_seconds %d: error %q lacks key and range", seconds, err)
		}
	}
}
