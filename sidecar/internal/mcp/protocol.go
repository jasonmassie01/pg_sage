package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

// Protocol versions. pg_sage is a dual-era server: clients that open with
// initialize get the negotiated handshake-era revision; requests carrying
// a protocol version in _meta are served statelessly (2026-07-28).
const (
	ModernVersion       = "2026-07-28"
	LatestLegacyVersion = "2025-11-25"
	metaVersionKey      = "io.modelcontextprotocol/protocolVersion"
	metaServerInfoKey   = "io.modelcontextprotocol/serverInfo"
	metaSubscriptionKey = "io.modelcontextprotocol/subscriptionId"
	listTTLMillis       = 60000
)

// SupportedVersions lists every protocol version the server speaks.
var SupportedVersions = []string{ModernVersion, LatestLegacyVersion, "2025-06-18",
	"2025-03-26"}

const serverInstructions = "pg_sage is an AI DBA for PostgreSQL. Name the monitored " +
	"database on every call (list_databases shows them; with one database it is the " +
	"default). Read tools are safe. Propose tools never bypass pg_sage's policy gate, and " +
	"approvals stay with a person. For a finding whose fix belongs in the application, " +
	"get_source_fix_packet gives a cited change to open as a pull request; report it with " +
	"report_source_fix and pg_sage verifies it after the deploy."

// knownMethods are the request methods the server answers.
var knownMethods = map[string]bool{"ping": true, "initialize": true,
	"server/discover": true, "tools/list": true, "tools/call": true,
	"subscriptions/listen": true}

func supportedVersion(version string) bool {
	for _, v := range SupportedVersions {
		if v == version {
			return true
		}
	}
	return false
}

// requestMeta is the protocol metadata of a request's params._meta.
type requestMeta struct {
	version string
	modern  bool
}

func parseMeta(params json.RawMessage) requestMeta {
	var holder struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if len(params) == 0 || json.Unmarshal(params, &holder) != nil {
		return requestMeta{}
	}
	raw, ok := holder.Meta[metaVersionKey]
	if !ok {
		return requestMeta{}
	}
	var version string
	_ = json.Unmarshal(raw, &version)
	return requestMeta{version: version, modern: true}
}

// Handle answers one JSON-RPC message. It returns nil for a notification
// (no id, or any notifications/* method): JSON-RPC forbids replying to
// one, and over stdio a stray line would be read as the next response.
func (s *Server) Handle(ctx context.Context, raw json.RawMessage) []byte {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' {
		return encodeResponse(rpcResponse{JSONRPC: "2.0",
			Error: failure(codeInvalidRequest, "batch requests are not supported")})
	}
	var request rpcRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return encodeResponse(rpcResponse{JSONRPC: "2.0", Error: failure(codeParse,
			"parse error")})
	}
	if request.JSONRPC != "2.0" || request.Method == "" {
		return encodeResponse(rpcResponse{JSONRPC: "2.0", ID: request.ID,
			Error: failure(codeInvalidRequest, "invalid request")})
	}
	if len(request.ID) == 0 || strings.HasPrefix(request.Method, "notifications/") {
		return nil
	}
	response := rpcResponse{JSONRPC: "2.0", ID: request.ID}
	meta := parseMeta(request.Params)
	if meta.modern && !supportedVersion(meta.version) {
		response.Error = unsupportedVersion(meta.version)
		return encodeResponse(response)
	}
	result, failed := s.dispatch(ctx, request.Method, request.Params, meta)
	if failed != nil {
		response.Error = failed
		return encodeResponse(response)
	}
	if meta.modern {
		result = s.modernResult(result)
	}
	response.Result = result
	return encodeResponse(response)
}

func unsupportedVersion(requested string) *rpcError {
	return &rpcError{Code: codeUnsupportedProto, Message: "Unsupported protocol version",
		Data: map[string]any{"supported": SupportedVersions, "requested": requested}}
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage,
	meta requestMeta) (map[string]any, *rpcError) {
	switch method {
	case "ping":
		return map[string]any{}, nil
	case "initialize":
		return s.initializeResult(params), nil
	case "server/discover":
		return s.discoverResult(), nil
	case "tools/list":
		return s.toolsList(ctx, meta.modern), nil
	case "tools/call":
		return s.callTool(ctx, params)
	case "subscriptions/listen":
		return nil, failure(codeMethodNotFound, "subscriptions/listen needs a streaming "+
			"transport (stdio or Streamable HTTP)")
	}
	return nil, failure(codeMethodNotFound, "method not found")
}

func (s *Server) serverInfo() map[string]string {
	return map[string]string{"name": "pg_sage", "version": s.version}
}

func (s *Server) capabilities() map[string]any {
	return map[string]any{"tools": map[string]any{"listChanged": true}}
}

func (s *Server) initializeResult(params json.RawMessage) map[string]any {
	var request struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &request)
	version := LatestLegacyVersion
	if request.ProtocolVersion != ModernVersion && supportedVersion(request.ProtocolVersion) {
		version = request.ProtocolVersion
	}
	return map[string]any{"protocolVersion": version, "capabilities": s.capabilities(),
		"serverInfo": s.serverInfo(), "instructions": serverInstructions}
}

func (s *Server) discoverResult() map[string]any {
	return map[string]any{"supportedVersions": SupportedVersions,
		"capabilities": s.capabilities(), "instructions": serverInstructions,
		"ttlMs": listTTLMillis, "cacheScope": "private"}
}

// modernResult adds the fields every 2026-07-28 result carries.
func (s *Server) modernResult(result map[string]any) map[string]any {
	if result == nil {
		result = map[string]any{}
	}
	meta, _ := result["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[metaServerInfoKey] = s.serverInfo()
	result["_meta"], result["resultType"] = meta, "complete"
	return result
}

// toolsList lists the tools the caller may call, each with the databases
// it may name, in registration order.
func (s *Server) toolsList(ctx context.Context, modern bool) map[string]any {
	names := s.permittedDatabaseNames(ctx)
	tools := make([]Tool, 0, len(s.tools))
	for _, tool := range s.tools {
		scope, _ := RequiredScope(tool.Name, nil)
		if !mayCall(ctx, scope) {
			continue
		}
		tools = append(tools, withDatabaseEnum(tool, names))
	}
	result := map[string]any{"tools": tools}
	if modern {
		result["ttlMs"], result["cacheScope"] = listTTLMillis, "private"
	}
	return result
}

// KnownMethod reports whether the server answers method (transports
// return 404 for an unknown modern method).
func KnownMethod(method string) bool { return knownMethods[method] }
