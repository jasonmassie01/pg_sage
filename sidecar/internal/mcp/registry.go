package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ToolAnnotations are the MCP behavior hints of a tool. No pg_sage tool
// destroys anything: mutations are proposals the policy gate decides.
type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

const databaseDescription = "Monitored database name (see list_databases). Required " +
	"when more than one database is monitored; defaults to the only one otherwise."

// toolDefinitions is the registry: every tool, in a fixed order, with the
// `database` argument and annotations applied.
func toolDefinitions() []Tool {
	tools := append(append(intentTools(), sreTools()...), sreActionTools()...)
	tools = append(append(tools, signalTools()...), runbookTools()...)
	tools = append(append(tools, autonomyTools()...), factTools()...)
	tools = append(append(append(tools, agentTools()...), specialistTools()...),
		askTools()...)
	tools = append(append(append(tools, fleetTools()...), agentBrokerTools()...),
		grantTools()...)
	for i := range tools {
		tools[i] = finishTool(tools[i])
	}
	return tools
}

func finishTool(tool Tool) Tool {
	if !fleetWideTool(tool.Name) {
		tool.InputSchema = setDatabaseProperty(tool.InputSchema, nil)
	}
	readOnly := !approveScopeTools[tool.Name] && !proposeScopeTools[tool.Name] &&
		!askToolNames[tool.Name] // ask_sage may queue a proposal
	tool.Annotations = &ToolAnnotations{ReadOnlyHint: readOnly}
	return tool
}

// setDatabaseProperty gives a schema the standard `database` property,
// with an enum of names when names is non-nil.
func setDatabaseProperty(schema json.RawMessage, names []string) json.RawMessage {
	var object map[string]any
	if json.Unmarshal(schema, &object) != nil {
		return schema
	}
	properties, _ := object["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
	}
	database := map[string]any{"type": "string", "description": databaseDescription}
	if len(names) > 0 {
		database["enum"] = names
	}
	properties["database"] = database
	object["properties"] = properties
	raw, err := json.Marshal(object)
	if err != nil {
		return schema
	}
	return raw
}

// withDatabaseEnum is tool as listed to one caller: its database property
// enumerates the databases that caller may name.
func withDatabaseEnum(tool Tool, names []string) Tool {
	if names == nil || fleetWideTool(tool.Name) {
		return tool
	}
	tool.InputSchema = setDatabaseProperty(tool.InputSchema, names)
	return tool
}

// Fingerprint identifies the tool list as an unrestricted caller sees it;
// it changes when tools or the monitored databases change, which is when
// clients are told the list changed.
func (s *Server) Fingerprint() string {
	var names []string
	if s.directory != nil {
		names = []string{}
		for _, ref := range s.directory.Databases() {
			names = append(names, ref.Name)
		}
	}
	tools := make([]Tool, 0, len(s.tools))
	for _, tool := range s.tools {
		tools = append(tools, withDatabaseEnum(tool, sortedCopy(names)))
	}
	raw, _ := json.Marshal(tools)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// sortedCopy returns names sorted (nil stays nil).
func sortedCopy(names []string) []string {
	if names == nil {
		return nil
	}
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
