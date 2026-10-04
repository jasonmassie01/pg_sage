package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Streamable HTTP. Every JSON-RPC message is one POST; the server answers
// with one JSON object, 202 for a notification, or an SSE stream for
// subscriptions/listen. Handshake-era clients (2025-03-26..2025-11-25) are
// served as before; a request carrying a protocol version in _meta must
// also carry the matching MCP-Protocol-Version, Mcp-Method and (for
// tools/call) Mcp-Name headers.

const maxHTTPBody = 4 << 20

// devOrigins are the dashboard's development origins (same as the API).
var devOrigins = map[string]bool{"http://localhost:5173": true,
	"http://localhost:8080": true, "http://127.0.0.1:5173": true,
	"http://127.0.0.1:8080": true}

func (r *Runtime) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !originAllowed(request) {
		writeHTTPError(w, http.StatusForbidden, nil, failure(codeInvalidRequest,
			"origin not allowed"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxHTTPBody))
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, nil, failure(codeParse, "unreadable body"))
		return
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		writeHTTPError(w, http.StatusBadRequest, nil, failure(codeInvalidRequest,
			"batch requests are not supported"))
		return
	}
	var message envelope
	_ = json.Unmarshal(body, &message)
	if status, failed := r.checkHTTPRequest(request, message); failed != nil {
		writeHTTPError(w, status, message.ID, failed)
		return
	}
	if message.Method == "subscriptions/listen" && len(message.ID) > 0 {
		r.serveSubscription(w, request, message)
		return
	}
	r.answer(w, request, body)
}

func (r *Runtime) answer(w http.ResponseWriter, request *http.Request, body []byte) {
	response := r.server.Handle(request.Context(), body)
	if response == nil {
		w.WriteHeader(http.StatusAccepted) // notification: no body
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(response)
}

// checkHTTPRequest applies the transport rules: the version header of a
// handshake-era request, and the header/body agreement of a modern one.
func (r *Runtime) checkHTTPRequest(request *http.Request, message envelope) (int,
	*rpcError) {
	header := request.Header.Get("MCP-Protocol-Version")
	meta := parseMeta(message.Params)
	if !meta.modern {
		if header != "" && !supportedVersion(header) {
			return http.StatusBadRequest, failure(codeInvalidRequest,
				"unsupported MCP-Protocol-Version "+printableName(header))
		}
		return 0, nil
	}
	if failed := modernHeaderMismatch(request, message, meta, header); failed != nil {
		return http.StatusBadRequest, failed
	}
	if !supportedVersion(meta.version) {
		return http.StatusBadRequest, unsupportedVersion(meta.version)
	}
	if len(message.ID) > 0 && !KnownMethod(message.Method) {
		return http.StatusNotFound, failure(codeMethodNotFound, "method not found")
	}
	return 0, nil
}

func modernHeaderMismatch(request *http.Request, message envelope, meta requestMeta,
	version string) *rpcError {
	mismatch := func(detail string) *rpcError {
		return failure(codeHeaderMismatch, "Header mismatch: "+detail)
	}
	if version != meta.version {
		return mismatch("MCP-Protocol-Version must equal the request's protocol version")
	}
	if request.Header.Get("Mcp-Method") != message.Method {
		return mismatch("Mcp-Method must equal the request method")
	}
	if message.Method != "tools/call" {
		return nil
	}
	name, ok := decodeHeaderValue(request.Header.Get("Mcp-Name"))
	var params struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(message.Params, &params)
	if !ok || name == "" || name != params.Name {
		return mismatch("Mcp-Name must equal the tool name")
	}
	return nil
}

// decodeHeaderValue decodes a plain visible-ASCII value or the
// =?base64?...?= sentinel form; false for anything else.
func decodeHeaderValue(value string) (string, bool) {
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") &&
		len(value) >= len("=?base64??=") {
		raw, err := base64.StdEncoding.DecodeString(value[len("=?base64?") : len(value)-2])
		return string(raw), err == nil
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return "", false
		}
	}
	return value, true
}

// originAllowed refuses browser requests from other sites (DNS rebinding):
// no Origin, the server's own host, or a local dashboard dev origin.
func originAllowed(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" || devOrigins[origin] {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host != "" && strings.EqualFold(parsed.Host, request.Host)
}

func writeHTTPError(w http.ResponseWriter, status int, id json.RawMessage,
	failed *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encodeResponse(rpcResponse{JSONRPC: "2.0", ID: id, Error: failed}))
}
