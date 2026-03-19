package main

import (
	"log"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// API key authentication middleware
// ---------------------------------------------------------------------------

// authMiddleware checks for a valid Bearer token when apiKey is non-empty.
// If apiKey is empty, it passes all requests through (open access).
func authMiddleware(apiKey string, next http.Handler) http.Handler {
	if apiKey == "" {
		return next // auth disabled
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")

		if header == "" {
			log.Printf("[auth] rejected %s %s from %s: missing Authorization header", r.Method, r.URL.Path, clientIP(r))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"missing Authorization header"}`))
			return
		}

		token := strings.TrimPrefix(header, "Bearer ")
		if token == header || token != apiKey {
			// Either no "Bearer " prefix, or token doesn't match
			log.Printf("[auth] rejected %s %s from %s: invalid API key", r.Method, r.URL.Path, clientIP(r))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid API key"}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}
