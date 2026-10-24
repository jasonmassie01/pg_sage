package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// GET /api/v1/change-reviews is the Reviews page's list: one database's
// reviews (?database=name) or every database running Change Review merged
// (no database, or "all"), newest first, filtered by ?state= and
// ?pr_url= and cut to ?limit= (1-100, default 20). A database whose read
// fails is listed in unavailable and the list is partial, not failed.

const changeReviewFleetList = "/api/v1/change-reviews"

// registerChangeReviewFleetRoutes registers the fleet list.
func registerChangeReviewFleetRoutes(mux *http.ServeMux, reg *changereview.Registry) {
	if reg == nil {
		return
	}
	viewer := RequireRole("admin", "operator", "viewer")
	h := changeReviewHandlers{reg: reg}
	mux.Handle("GET "+changeReviewFleetList, viewer(http.HandlerFunc(h.fleetList)))
}

type fleetReviewList struct {
	Reviews     []json.RawMessage `json:"reviews"`
	Databases   []string          `json:"databases"`
	Unavailable []string          `json:"unavailable"`
	Partial     bool              `json:"partial"`
}

func (h changeReviewHandlers) fleetList(w http.ResponseWriter, r *http.Request) {
	f, ok := fleetListFilter(w, r)
	if !ok {
		return
	}
	database := r.URL.Query().Get("database")
	names, ok := h.fleetListDatabases(w, database)
	if !ok {
		return
	}
	named := database != "" && database != "all"
	out := fleetReviewList{Databases: names, Unavailable: []string{}}
	var parts [][]changereview.Review
	for _, name := range names {
		svc, err := h.reg.Service(name)
		var reviews []changereview.Review
		if err == nil {
			reviews, err = svc.List(r.Context(), f)
		}
		switch {
		case errors.Is(err, changereview.ErrInvalid) || (err != nil && named):
			changeReviewError(w, err)
			return
		case err != nil:
			slog.Warn("change review list unavailable", "database", name, "error", err)
			out.Unavailable = append(out.Unavailable, name)
			continue
		}
		parts = append(parts, reviews)
	}
	out.Partial = len(out.Unavailable) > 0
	views, err := renderReviewViews(mergeReviewLists(parts, listLimit(f)))
	if err != nil {
		changeReviewError(w, err)
		return
	}
	out.Reviews = views
	jsonResponse(w, out)
}

// fleetListFilter reads ?state=, ?pr_url= and ?limit=; the service
// validates their values.
func fleetListFilter(w http.ResponseWriter, r *http.Request) (changereview.ListFilter, bool) {
	q := r.URL.Query()
	f := changereview.ListFilter{PRURL: q.Get("pr_url"), State: q.Get("state")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > changereview.MaxListLimit {
			sreErrorCode(w, "limit must be an integer 1-"+
				strconv.Itoa(changereview.MaxListLimit), "invalid_request",
				http.StatusBadRequest)
			return f, false
		}
		f.Limit = n
	}
	return f, true
}

// fleetListDatabases is the named database, or every registered one.
func (h changeReviewHandlers) fleetListDatabases(w http.ResponseWriter, database string) (
	[]string, bool) {
	if database == "" || database == "all" {
		names := h.reg.Databases()
		if names == nil {
			names = []string{}
		}
		return names, true
	}
	if validateDatabaseParam(database) != nil {
		sreErrorCode(w, "invalid database name", "invalid_request", http.StatusBadRequest)
		return nil, false
	}
	if _, ok := h.reg.Get(database); !ok {
		sreErrorCode(w, "database not found or Change Review is not running for it",
			"not_found", http.StatusNotFound)
		return nil, false
	}
	return []string{database}, true
}

func listLimit(f changereview.ListFilter) int {
	if f.Limit == 0 {
		return changereview.DefaultListLimit
	}
	return f.Limit
}

// mergeReviewLists merges per-database lists (each newest first) into
// one, newest first with ties by id, cut to limit.
func mergeReviewLists(parts [][]changereview.Review, limit int) []changereview.Review {
	out := make([]changereview.Review, 0)
	for _, p := range parts {
		out = append(out, p...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func renderReviewViews(reviews []changereview.Review) ([]json.RawMessage, error) {
	views := make([]json.RawMessage, 0, len(reviews))
	for _, rv := range reviews {
		raw, err := changereview.RenderJSON(rv)
		if err != nil {
			return nil, err
		}
		views = append(views, raw)
	}
	return views, nil
}
