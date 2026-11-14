package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
)

func databasesHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, mgr.FleetStatus())
	}
}

// parseTimeParam parses an ISO-8601/RFC-3339 timestamp, returning zero
// time on parse failure so callers can treat it as "unset".
func parseTimeParam(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func valOrDefault(
	q map[string][]string, key, def string,
) string {
	if vals, ok := q[key]; ok && len(vals) > 0 && vals[0] != "" {
		return vals[0]
	}
	return def
}

func firstVal(q map[string][]string, key string) string {
	if vals, ok := q[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// isConnectionError returns true if the error indicates a
// database connectivity problem rather than a query-level issue.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	markers := []string{
		"closed pool",
		"connection refused",
		"connection reset",
		"broken pipe",
		"no such host",
		"i/o timeout",
		"context deadline exceeded",
		"connection timed out",
		"pool closed",
	}
	lower := strings.ToLower(msg)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
