package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// The contract an agent client reads from tools/list (mcp-01a): every
// tool says when to use it, what it returns and what it never does, in at
// most 400 characters, and every input schema is a valid JSON Schema
// 2020-12 document.

// No concurrent access tests: tool definitions are static data built once.
// No state transition tests: listing tools changes nothing.

const maxDescriptionLength = 400

// descriptionRule is mcp-01a's rule, stricter than ux-05f's lintDescription.
func descriptionRule(name, description string) error {
	switch {
	case len(description) > maxDescriptionLength:
		return fmt.Errorf("%s: description is %d characters, over %d", name,
			len(description), maxDescriptionLength)
	case !strings.Contains(description, "Use when"):
		return fmt.Errorf("%s: description %q does not say \"Use when\"", name, description)
	case !strings.Contains(description, "Returns"):
		return fmt.Errorf("%s: description %q does not say what it Returns", name, description)
	case !strings.Contains(description, "Never"):
		return fmt.Errorf("%s: description %q does not say what it Never does", name,
			description)
	}
	return lintDescription(name, description)
}

// contractTools are the tools an admin sees in tools/list.
func contractTools(t *testing.T) []Tool {
	t.Helper()
	server := NewServer(strictBackend{newAllToolsBackend()}).WithDirectory(fleetOf("orders"))
	response := invoke(t, server, strictContext("tools/list"),
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	raw, err := json.Marshal(response.Result)
	require.NoError(t, err)
	var listed struct {
		Tools []Tool `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(raw, &listed))
	return listed.Tools
}

func TestToolsContractListsEveryRegisteredToolOnce(t *testing.T) {
	listed := contractTools(t)
	defined := toolDefinitions()
	require.Equal(t, len(defined), len(listed))
	seen := map[string]bool{}
	for i, tool := range listed {
		require.False(t, seen[tool.Name], "%s listed twice", tool.Name)
		seen[tool.Name] = true
		require.Equal(t, defined[i].Name, tool.Name, "registry order")
	}
}

func TestToolsContractEveryToolMeetsTheDescriptionRule(t *testing.T) {
	var failed []string
	for _, tool := range contractTools(t) {
		if err := descriptionRule(tool.Name, tool.Description); err != nil {
			failed = append(failed, err.Error())
		}
	}
	require.Empty(t, failed, "tools failing the description rule:\n%s",
		strings.Join(failed, "\n"))
}

func TestToolsContractDescriptionRuleRejectsPlantedDescriptions(t *testing.T) {
	ok := "Use when you need X. Returns X. Never changes anything."
	require.NoError(t, descriptionRule("planted", ok))
	for planted, want := range map[string]string{
		"Read standing policy":                            "Use when",
		"Use when you need X. Never changes anything.":    "Returns",
		"Use when you need X. Returns X and changes it.":  "Never",
		"Use to read X. Returns X. Never changes a thing": "Use when",
		"": "Use when",
		ok + strings.Repeat("x", maxDescriptionLength-len(ok)+1): "over 400",
	} {
		err := descriptionRule("planted", planted)
		require.Error(t, err, planted)
		require.Contains(t, err.Error(), want, planted)
	}
	boundary := ok + strings.Repeat("x", maxDescriptionLength-len(ok))
	require.Len(t, boundary, maxDescriptionLength)
	require.NoError(t, descriptionRule("planted", boundary), "exactly 400 is allowed")
}

// glossaryPath is ux-06a's generated glossary: the words people and
// agents read come from one list.
const glossaryPath = "../../web/src/generated/glossary.json"

// apiWords are glossary "avoid" words that are also API field names or
// enum values an agent reads in results (proposal_id, verdict, parked,
// finding ids, "policy decision" records), so descriptions may name them.
var apiWords = map[string]bool{"proposal": true, "proposals": true, "verdict": true,
	"verdicts": true, "parked": true, "finding": true, "findings": true, "case": true,
	"cases": true, "candidate": true, "candidates": true, "recommendation": true,
	"recommendations": true, "policy decision": true}

func TestToolsContractDescriptionsAvoidRetiredWords(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(glossaryPath))
	require.NoError(t, err)
	var glossary map[string]struct {
		Avoid []string `json:"avoid"`
	}
	require.NoError(t, json.Unmarshal(raw, &glossary))
	checked := 0
	for _, entry := range glossary {
		for _, word := range entry.Avoid {
			if apiWords[word] {
				continue
			}
			checked++
			pattern := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(word) + `\b`)
			for _, tool := range toolDefinitions() {
				require.False(t, pattern.MatchString(tool.Description),
					"%s says the retired %q", tool.Name, word)
			}
		}
	}
	require.GreaterOrEqual(t, checked, 10, "the glossary's avoid list was read")
}

func TestToolsContractEverySchemaIsValidJSONSchema(t *testing.T) {
	for _, tool := range contractTools(t) {
		schema := decodeSchema(t, tool.InputSchema)
		require.Equal(t, "object", schema["type"], tool.Name)
		require.NoError(t, checkSchema(tool.Name, schema), tool.Name)
	}
}

func TestToolsContractSchemaCheckRejectsBrokenSchemas(t *testing.T) {
	for raw, want := range map[string]string{
		`{"type":"objekt"}`: "type",
		`{"type":"object","required":["a"],"properties":{}}`:  "required",
		`{"type":"object","propertes":{}}`:                    "keyword",
		`{"type":"string","pattern":"("}`:                     "pattern",
		`{"type":"string","enum":[]}`:                         "enum",
		`{"type":"object","oneOf":{}}`:                        "oneOf",
		`{"type":"object","properties":{"a":{"type":"zzz"}}}`: "type",
	} {
		var schema map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &schema))
		err := checkSchema("planted", schema)
		require.Error(t, err, raw)
		require.Contains(t, err.Error(), want, raw)
	}
}

func TestToolsContractToolsListMatchesTheRegistryDescriptions(t *testing.T) {
	ctx := operatorContext(context.Background())
	response := invoke(t, NewServer(&recordingBackend{}), ctx,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
	raw, err := json.Marshal(response.Result)
	require.NoError(t, err)
	for _, tool := range toolDefinitions() {
		require.Contains(t, string(raw), mustJSON(t, tool.Description), tool.Name)
	}
}
