package mcp

import (
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// evid-03a: the fact tools' input schemas enumerate the owner brief type
// and its database subject kind, so an MCP client can list and propose it.
// No concurrent access tests: the schemas are static values.

func factToolEnums(t *testing.T, name, property string) map[string]bool {
	t.Helper()
	for _, tool := range NewServer(&factRecordingBackend{}).Tools() {
		if tool.Name != name {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(tool.InputSchema, &schema))
		out := map[string]bool{}
		for _, v := range schema.Properties[property].Enum {
			out[v] = true
		}
		return out
	}
	t.Fatalf("tool %s is not listed", name)
	return nil
}

func TestFactToolsEnumerateTheOwnerBrief(t *testing.T) {
	for _, tool := range []string{"list_facts", "propose_fact"} {
		types := factToolEnums(t, tool, "type")
		require.True(t, types["owner_brief"], tool+" type enum lacks owner_brief")
		require.True(t, types["table_window"], tool+" type enum lost table_window")
	}
	kinds := factToolEnums(t, "propose_fact", "subject_kind")
	require.True(t, kinds["database"], "propose_fact subject_kind enum lacks database")
	require.True(t, kinds["slot"], "propose_fact subject_kind enum lost slot")
}
