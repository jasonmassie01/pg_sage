package api

import (
	"net/http"
	"strings"
)

// decisionSignalMiddleware publishes EventDecisions on b after every
// successful write (POST, PUT, PATCH or DELETE answered below 400): a
// person approving, rejecting or ratifying an item is such a write, so a
// waiting agent (MCP wait_for_decision, mcp-01b) re-reads its item at once
// instead of after the poller's next change mark. Reads publish nothing;
// a nil broker publishes nothing. MCP calls are left out: no MCP tool
// decides a held item, and a finished wait must not wake every other
// waiter.
func decisionSignalMiddleware(b *EventBroker, next http.Handler) http.Handler {
	if b == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !writeMethod(r.Method) || mcpPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		recorder := &accessStatusWriter{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		if status < http.StatusBadRequest {
			b.Publish(Event{Type: EventDecisions})
		}
	})
}

func mcpPath(p string) bool {
	return p == "/mcp" || strings.HasPrefix(p, "/mcp/") || p == mcpEndpointPath ||
		strings.HasPrefix(p, mcpEndpointPath+"/")
}

func writeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
