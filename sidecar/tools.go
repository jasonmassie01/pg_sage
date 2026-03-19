package main

import (
	"context"
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// Tool catalogue
// ---------------------------------------------------------------------------

var toolCatalogue = []Tool{
	{
		Name:        "diagnose",
		Description: "Ask pg_sage an interactive diagnostic question about your database",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"question": {"type": "string", "description": "The diagnostic question to ask pg_sage"}
			},
			"required": ["question"]
		}`),
	},
	{
		Name:        "briefing",
		Description: "Generate a health briefing of the current database state",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {},
			"required": []
		}`),
	},
	{
		Name:        "suggest_index",
		Description: "Get index suggestions for a specific table",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"table": {"type": "string", "description": "Table name (optionally schema-qualified)"}
			},
			"required": ["table"]
		}`),
	},
	{
		Name:        "review_migration",
		Description: "Review DDL / migration SQL for risks and issues",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"ddl": {"type": "string", "description": "The DDL / migration SQL to review"}
			},
			"required": ["ddl"]
		}`),
	},
}

// ---------------------------------------------------------------------------
// Tool dispatcher
// ---------------------------------------------------------------------------

func callTool(ctx context.Context, name string, args json.RawMessage) (ToolsCallResult, error) {
	switch name {
	case "diagnose":
		return toolDiagnose(ctx, args)
	case "briefing":
		return toolBriefing(ctx)
	case "suggest_index":
		return toolSuggestIndex(ctx, args)
	case "review_migration":
		return toolReviewMigration(ctx, args)
	default:
		return ToolsCallResult{
			Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("unknown tool: %s", name)}},
			IsError: true,
		}, nil
	}
}

// ---------------------------------------------------------------------------
// Individual tool implementations
// ---------------------------------------------------------------------------

func toolDiagnose(ctx context.Context, args json.RawMessage) (ToolsCallResult, error) {
	var p struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid arguments: " + err.Error()), nil
	}
	if p.Question == "" {
		return toolErr("question is required"), nil
	}

	result, err := queryTextFallback(ctx,
		"SELECT sage.diagnose($1)", []any{p.Question},
		// fallback: just return a helpful message
		"SELECT 'sage.diagnose() not available — ensure pg_sage extension is loaded'", nil,
	)
	if err != nil {
		return toolErr(err.Error()), nil
	}
	return toolOK(result), nil
}

func toolBriefing(ctx context.Context) (ToolsCallResult, error) {
	result, err := queryTextFallback(ctx,
		"SELECT sage.briefing()", nil,
		"SELECT 'sage.briefing() not available — ensure pg_sage extension is loaded'", nil,
	)
	if err != nil {
		return toolErr(err.Error()), nil
	}
	return toolOK(result), nil
}

func toolSuggestIndex(ctx context.Context, args json.RawMessage) (ToolsCallResult, error) {
	var p struct {
		Table string `json:"table"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid arguments: " + err.Error()), nil
	}
	if p.Table == "" {
		return toolErr("table is required"), nil
	}

	question := "suggest indexes for table " + sanitize(p.Table)
	result, err := queryTextFallback(ctx,
		"SELECT sage.diagnose($1)", []any{question},
		"SELECT 'sage.diagnose() not available — ensure pg_sage extension is loaded'", nil,
	)
	if err != nil {
		return toolErr(err.Error()), nil
	}
	return toolOK(result), nil
}

func toolReviewMigration(ctx context.Context, args json.RawMessage) (ToolsCallResult, error) {
	var p struct {
		DDL string `json:"ddl"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErr("invalid arguments: " + err.Error()), nil
	}
	if p.DDL == "" {
		return toolErr("ddl is required"), nil
	}

	question := "review this migration: " + p.DDL
	result, err := queryTextFallback(ctx,
		"SELECT sage.diagnose($1)", []any{question},
		"SELECT 'sage.diagnose() not available — ensure pg_sage extension is loaded'", nil,
	)
	if err != nil {
		return toolErr(err.Error()), nil
	}
	return toolOK(result), nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func queryTextFallback(ctx context.Context, primary string, primaryArgs []any, fallback string, fallbackArgs []any) (string, error) {
	var result string
	err := pool.QueryRow(ctx, primary, primaryArgs...).Scan(&result)
	if err == nil {
		return result, nil
	}
	if fallbackArgs == nil {
		fallbackArgs = []any{}
	}
	err2 := pool.QueryRow(ctx, fallback, fallbackArgs...).Scan(&result)
	if err2 != nil {
		return "", fmt.Errorf("primary: %v; fallback: %v", err, err2)
	}
	return result, nil
}

func toolOK(text string) ToolsCallResult {
	return ToolsCallResult{Content: []ToolContent{{Type: "text", Text: text}}}
}

func toolErr(text string) ToolsCallResult {
	return ToolsCallResult{Content: []ToolContent{{Type: "text", Text: text}}, IsError: true}
}

func unmarshalToolsCall(raw json.RawMessage) (string, json.RawMessage, error) {
	var p ToolsCallParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", nil, err
	}
	if p.Name == "" {
		return "", nil, fmt.Errorf("tool name is required")
	}
	return p.Name, p.Arguments, nil
}
