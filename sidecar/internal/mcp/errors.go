package mcp

// JSON-RPC protocol error codes and the codes of tool execution errors.
// Since MCP 2025-06-18 a failed tool call that the model can act on
// (invalid arguments, a refused scope or database, a missing object) is a
// tools/call *result* with isError true; its structuredContent carries
// the code below so clients can tell the failures apart.
const (
	codeParse            = -32700
	codeInvalidRequest   = -32600
	codeMethodNotFound   = -32601
	codeInvalidParams    = -32602
	codeInternal         = -32603
	codeCancelled        = -32800
	codeHeaderMismatch   = -32020
	codeUnsupportedProto = -32022

	codeScopeRequired    = -32001
	codeNotPermitted     = -32003
	codeNotFound         = -32004
	codeApprovalReserved = -32005
	codeDatabaseRequired = -32006
	codeUnknownDatabase  = -32007
	codeConflict         = -32009
	codeUnavailable      = -32010
	codeRunbookRejected  = -32022
)

// errorReasons name each tool execution error code.
var errorReasons = map[int]string{
	codeScopeRequired: "scope_required", codeNotPermitted: "database_not_permitted",
	codeNotFound: "not_found", codeApprovalReserved: "approval_reserved_for_humans",
	codeDatabaseRequired: "database_required", codeUnknownDatabase: "unknown_database",
	codeConflict: "conflict", codeUnavailable: "unavailable",
	codeRunbookRejected: "runbook_rejected", codeInvalidParams: "invalid_arguments",
	codeInternal: "internal_error",
}

// toolError turns a tool's failure into an isError result.
func toolError(failed *rpcError) map[string]any {
	reason, ok := errorReasons[failed.Code]
	if !ok {
		reason = "error"
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": failed.Message}},
		"isError": true,
		"structuredContent": map[string]any{"error": map[string]any{
			"code": failed.Code, "reason": reason, "message": failed.Message}},
	}
}
