package api

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/whynot"
)

// GET /api/v1/whynot?database=&finding_id=|object=schema.name|category=
// answers "why has pg_sage not done this?" as one chain of ten links, from
// observing the database to the emergency stop (ux-08a). With only a
// database it answers for the whole database: why nothing has been
// proposed. Like the policy explain route it is an operator route over
// the visible databases, records no decision and takes no budget slot;
// it never returns SQL text. A link it cannot read is unknown with an
// error class, and the answer is still 200.

const whyNotPath = "/api/v1/whynot"

// whyNotQuery is one question: a finding, an object (with an optional
// category), a category, or the whole database.
type whyNotQuery struct {
	findingID int64
	object    string
	category  string
}

type whyNotResponse struct {
	Database  string `json:"database"`
	FindingID string `json:"finding_id,omitempty"`
	Object    string `json:"object,omitempty"`
	Category  string `json:"category,omitempty"`
	whynot.Result
}

var (
	whyNotObjectPattern = regexp.MustCompile(
		`^[A-Za-z_][A-Za-z0-9_$]{0,62}\.[A-Za-z_][A-Za-z0-9_$]{0,62}$`)
	whyNotCategoryPattern = regexp.MustCompile(`^[a-z][a-z0-9_:.-]{0,63}$`)
)

// validWhyNotObject accepts an unquoted schema.name of PostgreSQL
// identifiers (at most 63 bytes each).
func validWhyNotObject(object string) bool {
	return whyNotObjectPattern.MatchString(object)
}

// validWhyNotCategory accepts a stored category name.
func validWhyNotCategory(category string) bool {
	return whyNotCategoryPattern.MatchString(category)
}

// parseWhyNotQuery reads the question; a bad or conflicting parameter is
// an error naming it.
func parseWhyNotQuery(r *http.Request) (whyNotQuery, error) {
	values := r.URL.Query()
	var q whyNotQuery
	q.object, q.category = values.Get("object"), values.Get("category")
	if raw := values.Get("finding_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return q, errors.New("finding_id must be a positive integer")
		}
		if q.object != "" || q.category != "" {
			return q, errors.New("finding_id names one finding: give it without " +
				"object or category")
		}
		q.findingID = id
	}
	if q.object != "" && !validWhyNotObject(q.object) {
		return q, errors.New("object must be an unquoted schema.name")
	}
	if q.category != "" && !validWhyNotCategory(q.category) {
		return q, errors.New("category must be a finding category name")
	}
	return q, nil
}

func whyNotHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		q, err := parseWhyNotQuery(r)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		selected, ok := resolveSingleDatabaseRequestPool(mgr, dbName)
		if !ok {
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		if selected.pool == nil {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		rows, err := readWhyNotFindings(r.Context(), selected, q)
		if q.findingID > 0 && errors.Is(err, pgx.ErrNoRows) {
			jsonError(w, "finding not found", http.StatusNotFound)
			return
		}
		if err != nil {
			slog.Warn("whynot: finding read failed", "database", selected.name,
				"error", err)
		}
		in := gatherWhyNot(r, mgr, selected, q, rows, err)
		out := whyNotResponse{Database: selected.name, Object: q.object,
			Category: q.category, Result: whynot.Chain(r.Context(), in)}
		if q.findingID > 0 {
			out.FindingID = strconv.FormatInt(q.findingID, 10)
		}
		writeJSONStatus(w, http.StatusOK, out)
	}
}
