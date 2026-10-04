package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"unicode"
)

// Server is pg_sage's MCP server: protocol handling, tool registry,
// scope and database checks, and routing to the backends.
type Server struct {
	backend   Backend
	tools     []Tool
	directory Directory
	version   string
}

// NewServer returns a server over backend. Backends that also implement
// the optional tool interfaces (facts, investigations, coding-agent
// tools, ...) serve those tools; the others report them unavailable.
func NewServer(backend Backend) *Server {
	return &Server{backend: backend, tools: toolDefinitions(), version: "dev"}
}

// WithDirectory sets the fleet the server validates `database` against.
func (s *Server) WithDirectory(directory Directory) *Server {
	s.directory = directory
	return s
}

// WithVersion sets the version reported in serverInfo.
func (s *Server) WithVersion(version string) *Server {
	if version != "" {
		s.version = version
	}
	return s
}

// Tools returns every tool definition (without per-caller database enums).
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
	Data    any    `json:"data,omitempty"`
}

// toolFamily routes one family of tools.
type toolFamily func(s *Server, ctx context.Context, name string,
	arguments json.RawMessage) (any, *rpcError)

func familyOf(name string) toolFamily {
	switch _, runbook := runbookToolNames[name]; {
	case sreToolNames[name] || runbook:
		return (*Server).callSRETool
	case signalToolNames[name]:
		return (*Server).callSignalTool
	case sreActionToolNames[name]:
		return (*Server).callSREActionTool
	case autonomyToolNames[name]:
		return (*Server).callAutonomyTool
	case factToolNames[name]:
		return (*Server).callFactTool
	case specialistToolNames[name]:
		return (*Server).callSpecialistTool
	case agentToolNames[name]:
		return (*Server).callAgentTool
	case askToolNames[name]:
		return (*Server).callAskTool
	case name == "list_databases":
		return (*Server).listDatabases
	}
	return (*Server).callIntentTool
}

// callTool answers tools/call. Protocol problems (an unknown tool,
// params that are not a call) are JSON-RPC errors; everything the caller
// can correct is an isError result.
func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (map[string]any,
	*rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil || call.Name == "" {
		return nil, failure(codeInvalidParams, "invalid tool call: name is required")
	}
	if !knownTool(call.Name) {
		return nil, failure(codeInvalidParams, "Unknown tool: "+printableName(call.Name))
	}
	arguments, ok := objectArguments(call.Arguments)
	if !ok {
		return nil, failure(codeInvalidParams, "tool arguments must be a JSON object")
	}
	if failed := authorizeTool(ctx, call.Name, arguments); failed != nil {
		return toolError(failed), nil
	}
	ctx, arguments, failed := s.bindDatabase(ctx, call.Name, arguments)
	if failed != nil {
		return toolError(failed), nil
	}
	result, failed := familyOf(call.Name)(s, ctx, call.Name, arguments)
	if failed != nil {
		if failed.Code == codeCancelled {
			return nil, failed
		}
		return toolError(failed), nil
	}
	if structured, ok := result.(map[string]any); ok {
		return structured, nil
	}
	return toolSuccess(result), nil
}

// objectArguments returns the arguments when they are a JSON object
// (absent or null count as {}).
func objectArguments(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage(`{}`), true
	}
	return trimmed, trimmed[0] == '{' && json.Valid(trimmed)
}

// printableName bounds an unknown tool name before it is echoed.
func printableName(name string) string {
	out := make([]rune, 0, 64)
	for _, r := range name {
		if len(out) == 64 {
			return string(out) + "..."
		}
		if unicode.IsPrint(r) && r != '<' && r != '>' {
			out = append(out, r)
		}
	}
	return string(out)
}

// toolSuccess is a tools/call result: the MCP content[] array with the
// result as one JSON text block, plus structuredContent for clients that
// read typed output.
func toolSuccess(result any) map[string]any {
	text, err := json.Marshal(result)
	if err != nil {
		text = []byte(`{"error":"result is not JSON-encodable"}`)
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": result,
	}
}

// toolResult wraps a tool's result, or maps its error.
func toolResult(result any, err error) (any, *rpcError) {
	if err == nil {
		return toolSuccess(result), nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, failure(codeCancelled, "request cancelled")
	}
	return nil, failure(codeInternal, "internal error")
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
func failure(code int, message string) *rpcError {
	return &rpcError{Code: code, Message: message}
}
func encodeResponse(response rpcResponse) []byte { raw, _ := json.Marshal(response); return raw }
