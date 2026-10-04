package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/facts"
)

func (s *Server) callAgentTool(ctx context.Context, name string,
	arguments json.RawMessage) (any, *rpcError) {
	backend, ok := s.backend.(AgentToolBackend)
	if !ok {
		return nil, failure(codeInternal, "coding-agent tools unavailable")
	}
	call, failed := agentCalls[name](arguments)
	if failed != nil {
		return nil, failed
	}
	result, err := call(ctx, backend)
	if err != nil {
		return nil, agentFailure(err)
	}
	if packet, ok := result.(agenttools.Packet); ok {
		return packetResult(packet), nil
	}
	return toolSuccess(result), nil
}

// agentInvocation runs one validated coding-agent tool call.
type agentInvocation func(context.Context, AgentToolBackend) (any, error)

var agentCalls = map[string]func(json.RawMessage) (agentInvocation, *rpcError){
	"top_queries": topQueriesCall, "explain_query": explainCall,
	"whatif_index": whatIfCall, "lint_migration": lintCall,
	"query_sources": sourcesCall, "mark_object": markCall,
	"get_source_fix_packet": packetCall, "report_source_fix": reportCall,
}

func invalid(detail string) *rpcError {
	return failure(codeInvalidParams, "invalid arguments: "+detail)
}

func topQueriesCall(raw json.RawMessage) (agentInvocation, *rpcError) {
	var args struct {
		Database     string  `json:"database"`
		Limit        *int    `json:"limit"`
		OrderBy      *string `json:"order_by"`
		IncludePlans *bool   `json:"include_plans"`
	}
	if !decodeStrict(raw, &args) {
		return nil, invalid("top_queries takes limit, order_by and include_plans")
	}
	request := agenttools.TopQueriesRequest{IncludePlans: true}
	if args.Limit != nil {
		if *args.Limit < 1 || *args.Limit > 50 {
			return nil, invalid("limit must be 1-50")
		}
		request.Limit = *args.Limit
	}
	if args.OrderBy != nil {
		switch *args.OrderBy {
		case "total_time", "mean_time", "calls":
			request.OrderBy = *args.OrderBy
		default:
			return nil, invalid("order_by is total_time, mean_time or calls")
		}
	}
	if args.IncludePlans != nil {
		request.IncludePlans = *args.IncludePlans
	}
	return func(ctx context.Context, b AgentToolBackend) (any, error) {
		return b.TopQueries(ctx, request)
	}, nil
}

func explainCall(raw json.RawMessage) (agentInvocation, *rpcError) {
	var args struct {
		Database string              `json:"database"`
		Query    *string             `json:"query"`
		QueryID  *agenttools.QueryID `json:"query_id"`
		Analyze  bool                `json:"analyze"`
		Params   []string            `json:"params"`
	}
	if !decodeStrict(raw, &args) {
		return nil, invalid("explain_query takes query or query_id, analyze and params")
	}
	if (args.Query == nil) == (args.QueryID == nil) {
		return nil, invalid("give exactly one of query and query_id")
	}
	request := agenttools.ExplainRequest{Analyze: args.Analyze, Params: args.Params}
	if args.Query != nil {
		if strings.TrimSpace(*args.Query) == "" || len(*args.Query) > 20000 {
			return nil, invalid("query must be 1-20000 characters")
		}
		request.Query = *args.Query
	} else {
		request.QueryID = *args.QueryID
	}
	if len(args.Params) > 100 {
		return nil, invalid("at most 100 params")
	}
	return func(ctx context.Context, b AgentToolBackend) (any, error) {
		return b.ExplainQuery(ctx, request)
	}, nil
}

func whatIfCall(raw json.RawMessage) (agentInvocation, *rpcError) {
	var args struct {
		Database string               `json:"database"`
		DDL      string               `json:"ddl"`
		QueryIDs []agenttools.QueryID `json:"query_ids"`
	}
	switch {
	case !decodeStrict(raw, &args):
		return nil, invalid("whatif_index takes ddl and query_ids")
	case strings.TrimSpace(args.DDL) == "" || len(args.DDL) > 2000:
		return nil, invalid("ddl is one CREATE INDEX statement (1-2000 characters)")
	case len(args.QueryIDs) > 20:
		return nil, invalid("at most 20 query_ids")
	}
	request := agenttools.WhatIfRequest{DDL: args.DDL, QueryIDs: args.QueryIDs}
	return func(ctx context.Context, b AgentToolBackend) (any, error) {
		return b.WhatIfIndex(ctx, request)
	}, nil
}

func lintCall(raw json.RawMessage) (agentInvocation, *rpcError) {
	var args struct {
		Database  string `json:"database"`
		SQL       string `json:"sql"`
		PGVersion int    `json:"pg_version"`
	}
	switch {
	case !decodeStrict(raw, &args):
		return nil, invalid("lint_migration takes sql and pg_version")
	case strings.TrimSpace(args.SQL) == "" || len(args.SQL) > 100000:
		return nil, invalid("sql must be 1-100000 characters")
	case args.PGVersion < 0:
		return nil, invalid("pg_version must be >= 0")
	}
	request := agenttools.LintRequest{SQL: args.SQL, PGVersion: args.PGVersion}
	return func(ctx context.Context, b AgentToolBackend) (any, error) {
		return b.LintMigration(ctx, request)
	}, nil
}

func sourcesCall(raw json.RawMessage) (agentInvocation, *rpcError) {
	var args struct {
		Database      string              `json:"database"`
		QueryID       *agenttools.QueryID `json:"query_id"`
		SampleSeconds int                 `json:"sample_seconds"`
	}
	if !decodeStrict(raw, &args) || args.SampleSeconds < 0 || args.SampleSeconds > 5 {
		return nil, invalid("query_sources takes query_id and sample_seconds (0-5)")
	}
	request := agenttools.SourcesRequest{SampleSeconds: args.SampleSeconds}
	if args.QueryID != nil {
		request.QueryID = *args.QueryID
	}
	return func(ctx context.Context, b AgentToolBackend) (any, error) {
		return b.QuerySources(ctx, request)
	}, nil
}

// agentFailure maps a coding-agent tool's error to a distinguishable code
// without leaking internal detail.
func agentFailure(err error) *rpcError {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure(codeCancelled, "request cancelled")
	case errors.Is(err, agenttools.ErrNotFound):
		return failure(codeNotFound, "not found: "+err.Error())
	case errors.Is(err, agenttools.ErrInvalid), errors.Is(err, agenttools.ErrNoChange),
		factValidationError(err):
		return failure(codeInvalidParams, "invalid arguments: "+err.Error())
	case errors.Is(err, agenttools.ErrUnavailable):
		return failure(codeUnavailable, "unavailable: "+err.Error())
	case errors.Is(err, agenttools.ErrTransition):
		return failure(codeConflict, "conflict: "+err.Error())
	}
	return failure(codeInternal, "internal error")
}

func factValidationError(err error) bool {
	for _, target := range []error{facts.ErrInvalidType, facts.ErrInvalidKind,
		facts.ErrInvalidSubject, facts.ErrProtectedSubject, facts.ErrInvalidValue,
		facts.ErrNoEvidence, facts.ErrInvalidSource} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
