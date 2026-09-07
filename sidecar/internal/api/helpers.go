package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// errInvalidDatabaseParam is returned when the ?database= query
// string fails basic shape validation (bad charset or over length).
var errInvalidDatabaseParam = errors.New(
	"invalid database parameter")

func jsonResponse(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// internalError logs the underlying error server-side and replies
// with a generic 500 response. Callers should use this instead of
// passing raw err.Error() to jsonError for errors that may contain
// database internals, file paths, or connection strings — those
// details do not belong in a client-visible response.
//
// The op argument identifies the failed operation in the log
// (e.g. "list findings", "rotate api key") so the operator can
// correlate the log entry with the 500 the client saw.
func internalError(
	w http.ResponseWriter, r *http.Request, op string, err error,
) {
	slog.Error("api internal error",
		"op", op,
		"method", r.Method,
		"path", r.URL.Path,
		"err", err,
	)
	jsonError(w, "internal error", http.StatusInternalServerError)
}

// validateDatabaseParam performs a boundary shape check on an
// untrusted ?database= value. It rejects strings that are too long
// or contain characters not expected in a configured database alias
// (letters, digits, underscore, hyphen, dot). Empty is valid and
// means "no filter / primary". The special alias "all" is accepted.
//
// This is a cheap input-validation barrier — it is separate from the
// registry lookup in fleet.DatabaseManager. A value that passes this
// check may still be unknown to the manager; per-handler behavior
// (404 vs. empty response) is unchanged.
func validateDatabaseParam(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > 63 {
		return errInvalidDatabaseParam
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '.':
		default:
			return errInvalidDatabaseParam
		}
	}
	return nil
}

// readDatabaseParam extracts and validates the ?database= query
// param. On malformed input it writes a 400 response and returns
// false — callers should return immediately in that case.
func readDatabaseParam(
	w http.ResponseWriter, r *http.Request,
) (string, bool) {
	v := r.URL.Query().Get("database")
	if err := validateDatabaseParam(v); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return v, true
}

func responseDatabaseName(database string) string {
	if database == "" || database == "all" {
		return "all"
	}
	return database
}

func rejectUnknownDatabase(
	w http.ResponseWriter, mgr *fleet.DatabaseManager,
	database string,
) bool {
	if mgr == nil || database == "" || database == "all" {
		return false
	}
	if mgr.GetInstance(database) != nil {
		return false
	}
	jsonError(w, "database not found", http.StatusNotFound)
	return true
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return def
	}
	return v
}

type namedPool struct {
	name string
	pool *pgxpool.Pool
}

func poolsForDatabaseSelection(
	mgr *fleet.DatabaseManager, database string,
) []namedPool {
	if mgr == nil {
		return nil
	}
	if database != "" && database != "all" {
		pool := mgr.PoolForDatabase(database)
		if pool == nil {
			return nil
		}
		return []namedPool{{name: database, pool: pool}}
	}

	instances := mgr.Instances()
	names := make([]string, 0, len(instances))
	for name := range instances {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]namedPool, 0, len(names))
	for _, name := range names {
		inst := instances[name]
		if inst != nil && inst.Pool != nil {
			out = append(out, namedPool{name: name, pool: inst.Pool})
		}
	}
	return out
}

func resolveSingleDatabaseRequestPool(
	mgr *fleet.DatabaseManager, dbName string,
) (namedPool, bool) {
	if mgr == nil {
		return namedPool{}, true
	}
	if dbName != "" && dbName != "all" {
		inst := mgr.GetInstance(dbName)
		if inst == nil {
			return namedPool{}, true
		}
		return namedPool{name: dbName, pool: inst.Pool}, true
	}
	instances := mgr.Instances()
	if len(instances) == 1 {
		for name, inst := range instances {
			if inst == nil {
				return namedPool{name: name}, true
			}
			return namedPool{name: name, pool: inst.Pool}, true
		}
	}
	return namedPool{}, false
}
