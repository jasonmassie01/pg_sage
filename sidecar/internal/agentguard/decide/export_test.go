package decide

import "sort"

// ToolClassified and ClassifiedNonAgentTools expose toolClasses to the
// MCP tool census in package decide_test: mcp imports decide (through
// inbox and grants), so the census cannot run inside package decide.
func ToolClassified(name string) bool {
	_, known := toolClasses[name]
	return known
}

// ClassifiedNonAgentTools lists the classified tools that must be MCP
// tools (every entry but the agent-only tools), sorted.
func ClassifiedNonAgentTools() []string {
	var names []string
	for name := range toolClasses {
		if !agentOnlyTools[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
