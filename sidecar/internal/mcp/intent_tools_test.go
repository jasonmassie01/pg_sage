package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Tool descriptions say when to use a tool, what it returns and what it
// never does (ux-05f). The lint covers a pinned list; T7 mcp-01 widens it
// to every tool.

// No concurrent access tests: descriptions are static data.
// No state transition tests: listing tools changes nothing.

var lintedDescriptionTools = []string{"get_policy", "request_change", "get_ledger",
	"get_value", "optimize_query", "set_maintenance_policy", toolGetInbox, toolGetOutcomes}

// rewrittenDescriptionTools are the six ux-05f rewrote; each says what it
// never does.
var rewrittenDescriptionTools = lintedDescriptionTools[:6]

const minDescriptionLength = 40

// lintDescription is the description rule: long enough to say when to use
// the tool, and it says what the tool returns.
func lintDescription(name, description string) error {
	switch {
	case len(strings.TrimSpace(description)) < minDescriptionLength:
		return fmt.Errorf("%s: description %q is under %d characters", name, description,
			minDescriptionLength)
	case !strings.Contains(description, "Returns"):
		return fmt.Errorf("%s: description %q does not say what it Returns", name, description)
	}
	return nil
}

func toolIn(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %s is not defined", name)
	return Tool{}
}

func TestToolDescriptionLintPassesThePinnedTools(t *testing.T) {
	tools := toolDefinitions()
	for _, name := range lintedDescriptionTools {
		require.NoError(t, lintDescription(name, toolIn(t, tools, name).Description))
	}
}

func TestToolDescriptionLintRejectsAPlantedThinDescription(t *testing.T) {
	for _, planted := range []string{"Read standing policy", "", "   ",
		"Read the evidence ledger of every policy decision pg_sage made"} {
		require.Error(t, lintDescription("planted", planted), planted)
	}
	require.Contains(t, lintDescription("get_policy", "Read standing policy").Error(),
		"under 40 characters")
}

func TestRewrittenDescriptionsSayWhatTheyNeverDo(t *testing.T) {
	tools := toolDefinitions()
	for _, name := range rewrittenDescriptionTools {
		description := toolIn(t, tools, name).Description
		require.Contains(t, description, "never", name)
		require.Contains(t, description, "Use ", name)
	}
	ledger := toolIn(t, tools, "get_ledger").Description
	require.Contains(t, ledger, "policy decision")
	require.Contains(t, ledger, toolGetOutcomes, "results live in the outcome ledger")
	for _, name := range []string{"request_change", "optimize_query", "set_maintenance_policy"} {
		require.Contains(t, toolIn(t, tools, name).Description, "never approves", name)
	}
}

// request_change's intent object names the kinds pg_sage can plan.
func TestRequestChangeIntentSchemaNamesTheKinds(t *testing.T) {
	schema := decodeSchema(t, toolIn(t, toolDefinitions(), "request_change").InputSchema)
	properties := objectMap(t, schema["properties"])
	intent := objectMap(t, properties["intent"])
	require.Equal(t, "object", intent["type"])
	description, _ := intent["description"].(string)
	for _, kind := range []string{"optimize_query", "ensure_fk_indexes", "apply_migration",
		"declare_table_contract", "register_consumer"} {
		_, _, err := intentContract(kind, nil)
		require.NoError(t, err, "%s is a plannable kind", kind)
		require.Contains(t, description, kind)
	}
	require.Contains(t, description, "kind")
	kind := objectMap(t, objectMap(t, intent["properties"])["kind"])
	require.Equal(t, []any{"optimize_query", "ensure_fk_indexes", "apply_migration",
		"declare_table_contract", "register_consumer"}, kind["enum"])
	require.Equal(t, []any{"kind"}, intent["required"])
	_, _, err := intentContract("drop_table", nil)
	require.Error(t, err, "a kind outside the enum is one pg_sage cannot plan")
}

func TestToolsListShowsTheRewrittenDescriptions(t *testing.T) {
	response := invoke(t, NewServer(&recordingBackend{}), operatorContext(context.Background()),
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	raw, err := json.Marshal(response.Result)
	require.NoError(t, err)
	var listed struct {
		Tools []Tool `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(raw, &listed))
	defined := toolDefinitions()
	for _, name := range lintedDescriptionTools {
		description := toolIn(t, listed.Tools, name).Description
		require.Equal(t, toolIn(t, defined, name).Description, description, name)
		require.NoError(t, lintDescription(name, description))
	}
}
