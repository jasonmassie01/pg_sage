package api

import (
	"net/http"
	"strings"
)

// corsMiddleware handles CORS for local dev mode only.
// The dashboard is served from the same origin in production,
// so CORS headers are only needed when the Vite dev server
// runs on a different port. We restrict to localhost origins.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if isAllowedOrigin(origin) {
			w.Header().Set(
				"Access-Control-Allow-Origin", origin,
			)
			w.Header().Set(
				"Access-Control-Allow-Methods",
				"GET, POST, PUT, DELETE, OPTIONS",
			)
			w.Header().Set(
				"Access-Control-Allow-Headers",
				"Content-Type, Authorization",
			)
			w.Header().Set(
				"Access-Control-Allow-Credentials", "true",
			)
		}

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAllowedOrigin returns true for same-origin (empty) or
// localhost origins used during development.
func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	switch origin {
	case "http://localhost:5173",
		"http://localhost:8080",
		"http://127.0.0.1:5173",
		"http://127.0.0.1:8080":
		return true
	}
	return false
}

// maxBodyMiddleware wraps r.Body with a size limit for
// non-GET/HEAD requests to prevent memory exhaustion.
func maxBodyMiddleware(next http.Handler) http.Handler {
	const maxBody = 1 << 20 // 1 MB
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet &&
			r.Method != http.MethodHead &&
			r.Method != http.MethodOptions {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeadersMiddleware sets standard security headers
// on every response. HSTS and CSP are excluded as they
// require deployment-specific configuration.
func securityHeadersMiddleware(
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		w.Header().Set(
			"X-Content-Type-Options", "nosniff")
		w.Header().Set(
			"X-Frame-Options", "DENY")
		w.Header().Set(
			"Referrer-Policy",
			"strict-origin-when-cross-origin")
		w.Header().Set(
			"Permissions-Policy",
			"camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

// requireJSONMiddleware rejects POST/PUT/PATCH requests
// that do not send Content-Type: application/json.
func requireJSONMiddleware(
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		switch r.Method {
		case http.MethodPost, http.MethodPut,
			http.MethodPatch:
			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				jsonError(w,
					"Content-Type must be application/json",
					http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
