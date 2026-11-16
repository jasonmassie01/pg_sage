package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
)

// The workload evidence pack (evid-02c). get_workload_pack reads one
// pack's model tier: the redacted form a model may read. It never returns
// the local tier: statement texts as written, most-common values, plan
// conditions and the pseudonym map stay in pg_sage.

// WorkloadPackRequest are the typed arguments of get_workload_pack.
type WorkloadPackRequest struct {
	Database      string `json:"database,omitempty"`
	ContentSHA256 string `json:"content_sha256"`
}

// WorkloadPackBackend serves get_workload_pack.
type WorkloadPackBackend interface {
	WorkloadPackModel(context.Context, WorkloadPackRequest) (any, error)
}

// Errors a WorkloadPackBackend returns for the caller to tell apart.
var (
	ErrWorkloadPackNotFound = errors.New("workload pack not found")
	// ErrWorkloadPackRefused wraps a leak guard refusal; its message names
	// the section and field, never the value.
	ErrWorkloadPackRefused = errors.New("the model tier was refused")
)

var workloadPackToolNames = map[string]bool{"get_workload_pack": true}

var packSHAPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func workloadPackTools() []Tool {
	return []Tool{{Name: "get_workload_pack", Description: "Read a workload evidence " +
		"pack's model tier: the redacted form a model may read (numeric column " +
		"statistics, sanitized statement texts, plan shapes, schema and settings with " +
		"literals redacted, sparkline charts as PNG data URIs) and its size. It never " +
		"returns the local tier: no statement text as written, no most-common value, no " +
		"plan condition. List packs and build one in the dashboard or with POST " +
		"/api/v1/databases/{db}/workload-packs",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"content_sha256":{"type":"string","pattern":"^[0-9a-f]{64}$",` +
			`"description":"the pack's content hash (64 lower-case hex)"}},` +
			`"required":["content_sha256"],"additionalProperties":false}`)}}
}

func (s *Server) callWorkloadPackTool(ctx context.Context, _ string,
	raw json.RawMessage) (any, *rpcError) {
	backend, ok := s.backend.(WorkloadPackBackend)
	if !ok {
		return nil, failure(codeInternal, "workload packs unavailable")
	}
	var req WorkloadPackRequest
	if !decodeStrict(raw, &req) || !packSHAPattern.MatchString(req.ContentSHA256) {
		return nil, failure(codeInvalidParams, "invalid arguments: content_sha256 must "+
			"be 64 lower-case hex digits")
	}
	result, err := backend.WorkloadPackModel(ctx, req)
	switch {
	case errors.Is(err, ErrWorkloadPackNotFound):
		return nil, failure(-32004, "not found: "+err.Error())
	case errors.Is(err, ErrWorkloadPackRefused):
		return nil, failure(codeInternal, err.Error())
	}
	return toolResult(result, err)
}
