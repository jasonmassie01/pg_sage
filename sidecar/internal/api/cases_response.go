package api

import (
	"encoding/json"
	"net/http"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// casesHandler serves the selected databases' cases. A fleet request that
// cannot read some databases still answers 200 with the others' cases and
// names the missing databases in "unavailable"; a single-database request
// that fails answers 503 (cases-05). ?limit= and ?summary=1 return the top
// of the global rank without evidence payloads (cases-11b).
// ?finding_source= keeps one subsystem's findings (rules-12b).
func casesHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := casesDatabaseSelection(w, r, mgr)
		if !ok {
			return
		}
		focus := r.URL.Query().Get("case")
		if focus != "" && !validCaseIdentity(focus) {
			jsonError(w, "invalid case parameter", http.StatusBadRequest)
			return
		}
		opts, err := parseCasesListOptions(r.URL.Query())
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		source, ok := casesFindingSource(w, r.URL.Query())
		if !ok {
			return
		}
		report := emptyCasesReport()
		if mgr != nil {
			report = collectCases(withFindingSource(r.Context(), source), mgr, database)
		}
		if rejectSingleDatabaseFailure(w, database, report) {
			return
		}
		body := casesResponseBody(database, report)
		addFindingSourceFields(body, source)
		if focus != "" {
			body["focus"] = addFocusedCase(r.Context(), mgr, database, &report, focus)
			body["cases"], body["total"] = report.Cases, len(report.Cases)
		}
		applyCasesListOptions(body, report.Cases, opts)
		jsonResponse(w, body)
	}
}

// casesDatabaseSelection reads ?database= ("all" when absent) and rejects
// a malformed or unknown database.
func casesDatabaseSelection(
	w http.ResponseWriter, r *http.Request, mgr *fleet.DatabaseManager,
) (string, bool) {
	database, ok := readDatabaseParam(w, r)
	if !ok {
		return "", false
	}
	if database == "" {
		database = "all"
	}
	if rejectUnknownDatabase(w, mgr, database) {
		return "", false
	}
	return database, true
}

func emptyCasesReport() casesReport {
	return casesReport{
		Cases:       make([]cases.Case, 0),
		Unavailable: make([]casesUnavailable, 0),
		Truncated:   map[string]casesTruncation{},
	}
}

// rejectSingleDatabaseFailure answers 503 with the error class when the
// one database a request names could not be read. Fleet requests answer
// with what they have.
func rejectSingleDatabaseFailure(
	w http.ResponseWriter, database string, report casesReport,
) bool {
	if database == "all" || len(report.Unavailable) == 0 {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":       "cases unavailable for database " + database,
		"database":    database,
		"error_class": report.Unavailable[0].ErrorClass,
		"unavailable": report.Unavailable,
	})
	return true
}

func casesResponseBody(database string, report casesReport) map[string]any {
	return map[string]any{
		"database":    database,
		"cases":       report.Cases,
		"total":       len(report.Cases),
		"unavailable": report.Unavailable,
		"truncated":   report.Truncated,
	}
}
