package api

import (
	"log/slog"
	"net/http"
	"strconv"
)

// fleetReadFailure logs a per-database read failure and returns the
// client-safe entry reported under "errors" so a failing database is
// never presented as "nothing pending" (G9-B15).
func fleetReadFailure(database, op string, err error) map[string]string {
	slog.Error("fleet read failed",
		"database", database, "op", op, "error", err)
	return map[string]string{
		"database": database,
		"error":    op + " failed",
	}
}

// singleModeDatabaseFilter parses ?database= for the single-database
// pending handler. A numeric value filters by database id; a name is
// validated and, since there is only one database, applies no filter
// (G9-B27). Malformed values are rejected with 400.
func singleModeDatabaseFilter(
	w http.ResponseWriter, r *http.Request,
) (*int, bool) {
	raw := r.URL.Query().Get("database")
	if raw == "" || raw == "all" {
		return nil, true
	}
	if id, err := strconv.Atoi(raw); err == nil {
		return &id, true
	}
	if err := validateDatabaseParam(raw); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return nil, true
}
