package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Streamable HTTP. Every JSON-RPC message is one POST; the server answers
// with one JSON object, 202 for a notification, or an SSE stream for
// subscriptions/listen. Handshake-era clients (2025-03-26..2025-11-25) are
// served as before; a request carrying a protocol version in _meta must
// also carry the matching MCP-Protocol-Version, Mcp-Method and (for
// tools/call, the tool name; for resources/read, the uri) Mcp-Name headers.

const maxHTTPBody = 4 << 20

// waitWriteDeadline bounds the response of a wait_for_decision call.
const waitWriteDeadline = (MaxWaitSeconds + 15) * time.Second

// waitCall reports whether body is a tools/call of wait_for_decision.
func waitCall(body []byte) bool {
	var call struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	return json.Unmarshal(body, &call) == nil && call.Method == "tools/call" &&
		call.Params.Name == toolWaitForDecision
}

// devOrigins are the dashboard's development origins. The API's
// cross-origin check runs first and refuses their cross-port writes, so in
// practice they admit only requests proxied by the Vite dev server.
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
	if waitCall(body) {
		// wait_for_decision may wait up to a minute; the API server's
		// 60 s write timeout would cut the answer, so this response gets
		// its own bounded deadline (never an unbounded one).
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(waitWriteDeadline))
	}
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
	if message.Method != "tools/call" && message.Method != "resources/read" {
		return nil
	}
	name, ok := decodeHeaderValue(request.Header.Get("Mcp-Name"))
	var params struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
	}
	_ = json.Unmarshal(message.Params, &params)
	want, what := params.Name, "the tool name"
	if message.Method == "resources/read" {
		want, what = params.URI, "the resource uri"
	}
	if !ok || name == "" || name != want {
		return mismatch("Mcp-Name must equal " + what)
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
