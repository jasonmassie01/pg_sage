package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Prompt catalogue
// ---------------------------------------------------------------------------

var promptCatalogue = []Prompt{
	{
		Name:        "investigate_slow_query",
		Description: "Investigate why a specific query is slow",
		Arguments: []PromptArgument{
			{Name: "queryid", Description: "The query ID from pg_stat_statements", Required: true},
		},
	},
	{
		Name:        "review_schema",
		Description: "Review the schema design of a table",
		Arguments: []PromptArgument{
			{Name: "table", Description: "Table name (optionally schema-qualified)", Required: true},
		},
	},
	{
		Name:        "capacity_plan",
		Description: "Analyze current database capacity and growth trends",
		Arguments:   []PromptArgument{},
	},
}

// ---------------------------------------------------------------------------
// Prompt renderer
// ---------------------------------------------------------------------------

func getPrompt(name string, arguments map[string]string) (PromptsGetResult, error) {
	switch name {
	case "investigate_slow_query":
		qid, ok := arguments["queryid"]
		if !ok || qid == "" {
			return PromptsGetResult{}, fmt.Errorf("queryid argument is required")
		}
		return PromptsGetResult{
			Description: "Investigate slow query " + qid,
			Messages: []PromptMessage{
				{
					Role: "user",
					Content: ToolContent{
						Type: "text",
						Text: fmt.Sprintf(
							"Investigate why query %s is slow. Look at the execution plan, table statistics, and index usage.",
							sanitizePromptArg(qid),
						),
					},
				},
			},
		}, nil

	case "review_schema":
		table, ok := arguments["table"]
		if !ok || table == "" {
			return PromptsGetResult{}, fmt.Errorf("table argument is required")
		}
		return PromptsGetResult{
			Description: "Review schema for " + table,
			Messages: []PromptMessage{
				{
					Role: "user",
					Content: ToolContent{
						Type: "text",
						Text: fmt.Sprintf(
							"Review the schema design of table %s. Check for normalization issues, missing indexes, and type choices.",
							sanitizePromptArg(table),
						),
					},
				},
			},
		}, nil

	case "capacity_plan":
		return PromptsGetResult{
			Description: "Database capacity planning analysis",
			Messages: []PromptMessage{
				{
					Role: "user",
					Content: ToolContent{
						Type: "text",
						Text: "Analyze the current database capacity. Consider connection usage, storage growth, and query volume trends.",
					},
				},
			},
		}, nil

	default:
		return PromptsGetResult{}, fmt.Errorf("unknown prompt: %s", name)
	}
}

func sanitizePromptArg(s string) string {
	// Strip any control characters but allow general text
	var b strings.Builder
	for _, r := range s {
		if r >= 32 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func unmarshalPromptsGet(raw json.RawMessage) (string, map[string]string, error) {
	var p PromptsGetParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", nil, err
	}
	if p.Name == "" {
		return "", nil, fmt.Errorf("prompt name is required")
	}
	return p.Name, p.Arguments, nil
}
