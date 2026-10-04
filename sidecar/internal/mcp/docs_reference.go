package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Markers around the generated tool reference in docs/mcp.md.
const (
	ToolReferenceBegin = "<!-- BEGIN GENERATED MCP TOOL REFERENCE: regenerate with " +
		"`go test ./internal/mcp -run TestToolReferenceDocsMatchSchemas -update-docs` -->\n"
	ToolReferenceEnd = "<!-- END GENERATED MCP TOOL REFERENCE -->"
)

type schemaProperty struct {
	Type        any             `json:"type"`
	Description string          `json:"description"`
	Enum        []any           `json:"enum"`
	Const       any             `json:"const"`
	Pattern     string          `json:"pattern"`
	Format      string          `json:"format"`
	MinLength   *int            `json:"minLength"`
	MaxLength   *int            `json:"maxLength"`
	Minimum     *float64        `json:"minimum"`
	Maximum     *float64        `json:"maximum"`
	MaxItems    *int            `json:"maxItems"`
	Items       *schemaProperty `json:"items"`
}

// ToolReferenceMarkdown renders the tool reference from the tool schemas:
// one section per tool with its scope and an argument table.
func ToolReferenceMarkdown(tools []Tool) string {
	var b strings.Builder
	b.WriteString("\n")
	for _, tool := range tools {
		scope, _ := RequiredScope(tool.Name, nil)
		fmt.Fprintf(&b, "### `%s`\n\n%s.\n\nScope: `%s`", tool.Name,
			strings.TrimSuffix(tool.Description, "."), scope)
		if tool.Name == "request_change" {
			b.WriteString(" (`approve` when the intent kind is `declare_table_contract` " +
				"or `register_consumer`)")
		}
		b.WriteString("\n\n")
		writeArguments(&b, tool.InputSchema)
	}
	return b.String()
}

func writeArguments(b *strings.Builder, raw json.RawMessage) {
	var schema struct {
		Properties map[string]schemaProperty `json:"properties"`
		Required   []string                  `json:"required"`
	}
	_ = json.Unmarshal(raw, &schema)
	if len(schema.Properties) == 0 {
		b.WriteString("No arguments.\n\n")
		return
	}
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	b.WriteString("| Argument | Type | Required | Notes |\n|---|---|---|---|\n")
	for _, name := range names {
		p := schema.Properties[name]
		yes := "no"
		if required[name] {
			yes = "yes"
		}
		fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n", name, typeText(p), yes, notesText(p))
	}
	b.WriteString("\n")
}

func typeText(p schemaProperty) string {
	switch t := p.Type.(type) {
	case string:
		if t == "array" && p.Items != nil {
			return "array of " + typeText(*p.Items)
		}
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, part := range t {
			parts = append(parts, fmt.Sprint(part))
		}
		return strings.Join(parts, " or ")
	}
	if p.Const != nil {
		return "constant"
	}
	return "any"
}

func notesText(p schemaProperty) string {
	var notes []string
	if p.Description != "" {
		notes = append(notes, p.Description)
	}
	if len(p.Enum) > 0 {
		values := make([]string, 0, len(p.Enum))
		for _, v := range p.Enum {
			values = append(values, fmt.Sprintf("`%v`", v))
		}
		notes = append(notes, "one of "+strings.Join(values, ", "))
	}
	if p.Const != nil {
		notes = append(notes, fmt.Sprintf("must be `%v`", p.Const))
	}
	notes = append(notes, boundsText(p)...)
	return strings.ReplaceAll(strings.Join(notes, "; "), "|", "\\|")
}

func boundsText(p schemaProperty) []string {
	var notes []string
	if p.Format != "" {
		notes = append(notes, "format "+p.Format)
	}
	if p.Pattern != "" {
		notes = append(notes, "pattern `"+p.Pattern+"`")
	}
	if p.MinLength != nil || p.MaxLength != nil {
		notes = append(notes, "length "+rangeText(intPtr(p.MinLength), intPtr(p.MaxLength)))
	}
	if p.Minimum != nil || p.Maximum != nil {
		notes = append(notes, "value "+rangeText(p.Minimum, p.Maximum))
	}
	if p.MaxItems != nil {
		notes = append(notes, fmt.Sprintf("at most %d items", *p.MaxItems))
	}
	return notes
}

func intPtr(v *int) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

func rangeText(minimum, maximum *float64) string {
	switch {
	case minimum != nil && maximum != nil:
		return fmt.Sprintf("%g-%g", *minimum, *maximum)
	case minimum != nil:
		return fmt.Sprintf(">= %g", *minimum)
	}
	return fmt.Sprintf("<= %g", *maximum)
}
