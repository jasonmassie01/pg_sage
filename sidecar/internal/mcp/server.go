package mcp

import (
	"context"
	"encoding/json"
	"errors"
)

type Server struct {
	backend Backend
	tools   []Tool
}

func NewServer(backend Backend) *Server {
	return &Server{backend: backend, tools: intentTools()}
}

func (s *Server) Tools() []Tool { return append([]Tool(nil), s.tools...) }

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) Handle(ctx context.Context, raw json.RawMessage) []byte {
	var request rpcRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return encodeResponse(rpcResponse{JSONRPC: "2.0", Error: failure(-32700, "parse error")})
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return encodeResponse(rpcResponse{JSONRPC: "2.0", ID: request.ID,
			Error: failure(-32600, "invalid request")})
	}
	response := rpcResponse{JSONRPC: "2.0", ID: request.ID}
	switch request.Method {
	case "initialize":
		response.Result = map[string]any{"protocolVersion": "2025-03-26",
			"capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo":   map[string]string{"name": "pg_sage", "version": "1"}}
	case "tools/list":
		response.Result = map[string]any{"tools": s.Tools()}
	case "tools/call":
		response.Result, response.Error = s.callTool(ctx, request.Params)
	default:
		response.Error = failure(-32601, "method not found")
	}
	return encodeResponse(response)
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil || call.Name == "" {
		return nil, failure(-32602, "invalid tool arguments")
	}
	var result any
	var err error
	switch call.Name {
	case "get_policy":
		var request PolicyRequest
		if !decodeArguments(call.Arguments, &request) {
			return nil, failure(-32602, "invalid arguments")
		}
		result, err = s.backend.GetPolicy(ctx, request)
	case "propose_policy_change":
		var request PolicyProposalRequest
		if !decodeArguments(call.Arguments, &request) || emptyJSON(request.Delta) {
			return nil, failure(-32602, "delta is required")
		}
		result, err = s.backend.ProposePolicyChange(ctx, request)
	case "request_change":
		var request ChangeRequest
		if !decodeArguments(call.Arguments, &request) || emptyJSON(request.Intent) {
			return nil, failure(-32602, "intent is required")
		}
		result, err = s.backend.RequestChange(ctx, request)
	case "get_ledger":
		var request LedgerRequest
		if !decodeArguments(call.Arguments, &request) {
			return nil, failure(-32602, "invalid arguments")
		}
		result, err = s.backend.GetLedger(ctx, request)
	case "optimize_query", "apply_migration", "ensure_fk_indexes",
		"declare_table_contract", "register_consumer", "set_maintenance_policy",
		"get_guarantee_status", "get_value":
		backend, ok := s.backend.(IntentBackend)
		if !ok {
			return nil, failure(-32603, "internal error")
		}
		if len(call.Arguments) == 0 {
			call.Arguments = json.RawMessage(`{}`)
		}
		result, err = backend.RequestIntent(ctx, call.Name, call.Arguments)
	default:
		return nil, failure(-32601, "tool not found")
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, failure(-32800, "request cancelled")
		}
		return nil, failure(-32603, "internal error")
	}
	return map[string]any{"structuredContent": result}, nil
}

func decodeArguments(raw json.RawMessage, target any) bool {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	return json.Unmarshal(raw, target) == nil
}
func emptyJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null" || string(raw) == "{}"
}
func failure(code int, message string) *rpcError { return &rpcError{code, message} }
func encodeResponse(response rpcResponse) []byte { raw, _ := json.Marshal(response); return raw }

func intentTools() []Tool {
	object := func(properties string, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":` + properties +
			`,"required":` + required + `,"additionalProperties":false}`)
	}
	return []Tool{
		{Name: "get_policy", Description: "Read standing policy",
			InputSchema: object(`{"database_id":{"type":"integer"}}`, `[]`)},
		{Name: "propose_policy_change", Description: "Propose policy delta",
			InputSchema: object(`{"database_id":{"type":"integer"},"delta":{"type":"object"},`+
				`"caller_claims":{"type":"object"}}`, `["delta"]`)},
		{Name: "request_change", Description: "Request an intent-level database change",
			InputSchema: object(`{"database_id":{"type":"integer"},"intent":{"type":"object"},`+
				`"caller_claims":{"type":"object"}}`, `["intent"]`)},
		{Name: "optimize_query", Description: "Optimize a query under standing policy",
			InputSchema: object(`{"query_id":{"type":"integer"},"query_text":{"type":"string"},`+
				`"goal":{"const":"latency"},"constraints":{"type":"object"}}`, `["goal"]`)},
		{Name: "apply_migration", Description: "Plan and rehearse an online migration",
			InputSchema: object(`{"ddl":{"type":"string"},"intent":{"type":"object"},`+
				`"constraints":{"type":"object"}}`, `[]`)},
		{Name: "ensure_fk_indexes", Description: "Ensure foreign keys have supporting indexes",
			InputSchema: object(`{"schema":{"type":"string"}}`, `["schema"]`)},
		{Name: "declare_table_contract", Description: "Declare table intent constraints",
			InputSchema: object(`{"table":{"type":"string"},"append_only":{"type":"boolean"},`+
				`"retention":{"type":"object"},"expected_pk":{"type":"string"},`+
				`"exemptions":{"type":"array","items":{"type":"string"}}}`, `["table"]`)},
		{Name: "register_consumer", Description: "Protect a replication slot consumer",
			InputSchema: object(`{"slot_name":{"type":"string"},"owner":{"type":"string"}}`,
				`["slot_name","owner"]`)},
		{Name: "set_maintenance_policy", Description: "Propose a maintenance policy patch",
			InputSchema: object(`{"scope":{"type":"object"},"patch":{"type":"object"}}`,
				`["scope","patch"]`)},
		{Name: "get_guarantee_status", Description: "Read machine-readable invariant status",
			InputSchema: object(`{}`, `[]`)},
		{Name: "get_value", Description: "Read verified DBA-hours saved",
			InputSchema: object(`{}`, `[]`)},
		{Name: "get_ledger", Description: "Read evidence ledger",
			InputSchema: object(`{"filter":{"type":"object"}}`, `[]`)},
	}
}
