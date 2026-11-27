package mcp

import (
	"context"
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/agenttools"
)

// Coding-agent tools (roadmap phase 3, "MCP v2 for coding agents"): what
// Claude Code or Cursor needs to fix a database problem at its source in
// the application repository. All but mark_object and report_source_fix
// only read; none of them runs DDL.

// AgentToolBackend serves the coding-agent tools for the database the
// server resolved (DatabaseFromContext).
type AgentToolBackend interface {
	TopQueries(context.Context, agenttools.TopQueriesRequest) (agenttools.TopQueriesResult,
		error)
	ExplainQuery(context.Context, agenttools.ExplainRequest) (agenttools.ExplainResult, error)
	WhatIfIndex(context.Context, agenttools.WhatIfRequest) (agenttools.WhatIfResult, error)
	LintMigration(context.Context, agenttools.LintRequest) (agenttools.LintResult, error)
	QuerySources(context.Context, agenttools.SourcesRequest) (agenttools.SourcesResult, error)
	MarkObject(context.Context, agenttools.MarkRequest, string) (agenttools.MarkResult, error)
	SourceFixPacket(context.Context, int64) (agenttools.Packet, error)
	ReportSourceFix(context.Context, agenttools.ReportRequest, string) (agenttools.Report,
		error)
}

var agentToolNames = map[string]bool{"top_queries": true, "explain_query": true,
	"whatif_index": true, "lint_migration": true, "query_sources": true,
	"mark_object": true, "get_source_fix_packet": true, "report_source_fix": true}

const queryIDSchema = `{"type":["integer","string"],"pattern":"^-?[0-9]{1,20}$",` +
	`"description":"pg_stat_statements queryid (send large ids as a decimal string)"}`

func agentTools() []Tool {
	schema := func(properties, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":{` + properties +
			`},"required":` + required + `,"additionalProperties":false}`)
	}
	tools := []Tool{
		{Name: "list_databases", InputSchema: schema(``, `[]`)},
		{Name: "top_queries",
			InputSchema: schema(`"limit":{"type":"integer","minimum":1,"maximum":50},`+
				`"order_by":{"type":"string","enum":["total_time","mean_time","calls"]},`+
				`"include_plans":{"type":"boolean"}`, `[]`)},
		{Name: "explain_query",
			InputSchema: schema(`"query":{"type":"string","minLength":1,"maxLength":20000},`+
				`"query_id":`+queryIDSchema+`,"analyze":{"type":"boolean"},`+
				`"params":{"type":"array","maxItems":100,"items":{"type":"string",`+
				`"maxLength":1000}}`, `[]`)},
		{Name: "whatif_index",
			InputSchema: schema(`"ddl":{"type":"string","minLength":1,"maxLength":2000,`+
				`"description":"one CREATE INDEX statement"},"query_ids":{"type":"array",`+
				`"minItems":1,"maxItems":20,"description":"the workload queries to `+
				`measure (from top_queries)","items":`+queryIDSchema+`}`, `["ddl"]`)},
		{Name: "lint_migration",
			InputSchema: schema(`"sql":{"type":"string","minLength":1,"maxLength":100000},`+
				`"pg_version":{"type":"integer","minimum":0,"description":"a major `+
				`(14-18) or a server_version_num (140000-189999); 0 or absent reads `+
				`the server's"}`, `["sql"]`)},
		{Name: "query_sources",
			InputSchema: schema(`"query_id":`+queryIDSchema+`,"sample_seconds":`+
				`{"type":"integer","minimum":0,"maximum":5}`, `[]`)},
	}
	return append(tools, agentWriteTools(schema)...)
}

func agentWriteTools(schema func(string, string) json.RawMessage) []Tool {
	return []Tool{
		{Name: "mark_object",
			InputSchema: schema(`"subject_kind":{"type":"string","enum":["index","table",`+
				`"schema"]},"subject":{"type":"string","minLength":1,"maxLength":300},`+
				`"mark":{"type":"string","enum":["owned","exempt"]},"repo":{"type":"string",`+
				`"maxLength":300},"path":{"type":"string","maxLength":300},`+
				`"evidence":{"type":"string","minLength":1,"maxLength":500}`,
				`["subject_kind","subject","mark","evidence"]`)},
		{Name: "get_source_fix_packet",
			InputSchema: schema(`"finding_id":{"type":"integer","minimum":1}`,
				`["finding_id"]`)},
		{Name: "report_source_fix",
			InputSchema: schema(`"finding_id":{"type":"integer","minimum":1},`+
				`"stage":{"type":"string","enum":["pr_opened","deployed","status"]},`+
				`"pr_url":{"type":"string","format":"uri","maxLength":500},`+
				`"commit":{"type":"string","pattern":"^[0-9a-f]{7,64}$"},`+
				`"deployed_at":{"type":"string","format":"date-time"},`+
				`"packet_hash":{"type":"string","maxLength":128}`,
				`["finding_id","stage"]`)},
	}
}
