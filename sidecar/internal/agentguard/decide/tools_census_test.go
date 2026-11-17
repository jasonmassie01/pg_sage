package decide_test

import (
	"sort"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// §6.2.6: every existing MCP tool is mapped to a class, so a new tool
// fails this census until someone decides how agents may use it. It
// lives in package decide_test because mcp imports decide.
func TestEveryMCPToolIsClassified(t *testing.T) {
	var missing []string
	names := map[string]bool{}
	for _, tool := range mcp.NewServer(nil).Tools() {
		names[tool.Name] = true
		if !decide.ToolClassified(tool.Name) {
			missing = append(missing, tool.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("MCP tools without a §6.2.6 class (add them to toolClasses): %v", missing)
	}
	// And no stale entries: every classified tool exists.
	for _, name := range decide.ClassifiedNonAgentTools() {
		if !names[name] {
			t.Fatalf("toolClasses lists %q, which is not an MCP tool", name)
		}
	}
}

// The census must see the registry, or it would pass on an empty list.
func TestEveryMCPToolIsClassified_SeesTheRegistry(t *testing.T) {
	tools := mcp.NewServer(nil).Tools()
	if len(tools) < 50 || decide.ToolClassified("brand_new_tool") {
		t.Fatalf("census input: %d tools, unknown classified=%v; want a full registry and "+
			"an unknown tool unclassified", len(tools), decide.ToolClassified("brand_new_tool"))
	}
}
