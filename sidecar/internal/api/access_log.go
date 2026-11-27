package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/inbox"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// The API access log (apiauth-12) records who did what through the API:
// one line per mutating request and per 401/403, with the method, route
// pattern, user id, role, status and latency. Bodies and query strings are
// never logged.
//
// Two layers make it work. accessLogMiddleware sits outside the
// cross-origin check and auth, so it sees every refusal, and puts an
// accessEntry in the context. accessStampMiddleware wraps the API mux
// itself: ServeMux sets r.Pattern on the request it is handed, and auth has
// put the user in that request's context, so only the innermost layer can
// read both. It copies them into the entry the outer layer then logs.
// An MCP Bearer call has no session user: accessPrincipalMiddleware, below
// the Bearer check, records its token principal instead.

// accessLogMaxPath caps the path logged for a request refused before
// routing, which has no pattern.
const accessLogMaxPath = 200

type accessEntryKey struct{}

// accessLogOn reports api.access_log; with no config the log is on.
func accessLogOn(cfg *config.Config) bool { return cfg == nil || cfg.API.AccessLogEnabled() }

// accessEntry is what the inner layer learned about the request. actor is
// the MCP token principal ("token:<id>") of a Bearer call, which has no
// session user.
type accessEntry struct {
	pattern string
	userID  int
	role    string
	hasUser bool
	actor   string
}

// accessStatusWriter records the response status. Unwrap keeps
// http.ResponseController working, and Flush keeps the SSE stream working.
type accessStatusWriter struct {
	http.ResponseWriter
	status int
}

func (w *accessStatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessStatusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *accessStatusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *accessStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// accessLogMiddleware logs mutating requests and 401/403 responses.
func accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		entry := &accessEntry{}
		sw := &accessStatusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(),
			accessEntryKey{}, entry)))
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		if !accessLogged(r.Method, status) {
			return
		}
		logAccess(r, entry, status, time.Since(start))
	})
}

// accessStampMiddleware wraps the API mux and records the matched pattern
// and the authenticated user for accessLogMiddleware.
func accessStampMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		entry, ok := r.Context().Value(accessEntryKey{}).(*accessEntry)
		if !ok {
			return
		}
		entry.pattern = r.Pattern
		if user := UserFromContext(r.Context()); user != nil {
			entry.userID, entry.role, entry.hasUser = user.ID, user.Role, true
		}
	})
}

// accessPrincipalMiddleware wraps the MCP handler, below the Bearer
// check that binds the token principal, and records that principal for
// accessLogMiddleware: a Bearer call has no session user.
func accessPrincipalMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry, ok := r.Context().Value(accessEntryKey{}).(*accessEntry)
		if p, bound := mcp.PrincipalFromContext(r.Context()); ok && bound && p.Actor != "" {
			entry.actor, entry.role = p.Actor, p.Role
		}
		next.ServeHTTP(w, r)
	})
}

// accessLogged keeps the log to changes and refusals: reads that succeed
// or fail for other reasons are not logged.
func accessLogged(method string, status int) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

func logAccess(r *http.Request, entry *accessEntry, status int, latency time.Duration) {
	attrs := []any{"method", r.Method, "status", status,
		"latency_ms", latency.Milliseconds()}
	if entry.pattern != "" {
		attrs = append(attrs, "pattern", entry.pattern)
	} else {
		// Refused before routing (401, cross-origin 403): the path only,
		// never the query string.
		path := r.URL.Path
		if len(path) > accessLogMaxPath {
			path = path[:accessLogMaxPath]
		}
		attrs = append(attrs, "path", path)
	}
	if surface := r.Header.Get(inbox.SurfaceHeader); surface != "" {
		attrs = append(attrs, "decided_from", inbox.Surface(surface))
	}
	if entry.hasUser {
		attrs = append(attrs, "user_id", entry.userID, "role", entry.role)
	} else if entry.actor != "" {
		attrs = append(attrs, "actor", entry.actor, "role", entry.role)
	}
	slog.Info("api access", attrs...)
}
