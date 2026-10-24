package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// Change Review (pre-merge database review). Session auth: every signed-in
// role may submit a review, list and read reviews and read the summary;
// only operators and admins record a DBA verdict. A review that is still
// running answers 202; Submit and Get may wait up to 50 s for it. Nothing
// here runs the reviewed SQL, approves a change or merges anything.

const (
	changeReviewBasePath = "/api/v1/databases/{db}/change-reviews"
	changeReviewSummary  = "/api/v1/change-reviews/summary"
	defaultSummaryDays   = 30
	maxSummaryDays       = 365
	busyRetryAfter       = "30"
)

type changeReviewHandlers struct{ reg *changereview.Registry }

// registerChangeReviewRoutes registers every Change Review route on one
// mux (a mux without the managed-database routes).
func registerChangeReviewRoutes(mux *http.ServeMux, reg *changereview.Registry) {
	registerChangeReviewDatabaseRoutes(mux, reg)
	registerChangeReviewSummaryRoute(mux, reg)
	registerChangeReviewFleetRoutes(mux, reg)
}

// registerChangeReviewDatabaseRoutes registers the per-database routes.
// The router puts them on the per-database mux behind the {db} catch-all
// (sre_handlers.go): "{db}/change-reviews" and "managed/{id}" overlap
// with neither more specific, which ServeMux refuses on one mux.
func registerChangeReviewDatabaseRoutes(mux *http.ServeMux, reg *changereview.Registry) {
	if reg == nil {
		return
	}
	viewer := RequireRole("admin", "operator", "viewer")
	dba := RequireRole("admin", "operator")
	h := changeReviewHandlers{reg: reg}
	mux.Handle("POST "+changeReviewBasePath, viewer(http.HandlerFunc(h.submit)))
	mux.Handle("GET "+changeReviewBasePath, viewer(http.HandlerFunc(h.list)))
	mux.Handle("GET "+changeReviewBasePath+"/{id}", viewer(http.HandlerFunc(h.get)))
	mux.Handle("POST "+changeReviewBasePath+"/{id}/verdict", dba(http.HandlerFunc(h.verdict)))
}

// registerChangeReviewSummaryRoute registers the fleet summary.
func registerChangeReviewSummaryRoute(mux *http.ServeMux, reg *changereview.Registry) {
	if reg == nil {
		return
	}
	viewer := RequireRole("admin", "operator", "viewer")
	h := changeReviewHandlers{reg: reg}
	mux.Handle("GET "+changeReviewSummary, viewer(http.HandlerFunc(h.summary)))
}

// changeReviewActor is the session user's stable identity.
func changeReviewActor(r *http.Request) string {
	return fmt.Sprintf("user:%d", UserFromContext(r.Context()).ID)
}

// service resolves {db}; it writes the error response when it fails.
func (h changeReviewHandlers) service(w http.ResponseWriter, r *http.Request) (
	*changereview.Service, bool) {
	name := r.PathValue("db")
	if name == "" || name == "all" || validateDatabaseParam(name) != nil {
		sreErrorCode(w, "invalid database name", "invalid_request", http.StatusBadRequest)
		return nil, false
	}
	svc, err := h.reg.Service(name)
	if err != nil {
		sreErrorCode(w, "database not found or Change Review is not running for it",
			"not_found", http.StatusNotFound)
		return nil, false
	}
	return svc, true
}

// decodeStrictBody decodes a JSON object, refusing unknown fields and
// trailing data.
func decodeStrictBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

func (h changeReviewHandlers) submit(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	var req changereview.Request
	if err := decodeStrictBody(r, &req); err != nil {
		sreErrorCode(w, "body must be a change review request: "+err.Error(),
			"invalid_request", http.StatusBadRequest)
		return
	}
	review, created, err := svc.Submit(r.Context(), changereview.Caller{
		Actor: changeReviewActor(r), Source: changereview.SourceAPI}, req)
	if err != nil {
		changeReviewError(w, err)
		return
	}
	writeChangeReview(w, review, &created)
}

func (h changeReviewHandlers) list(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := changereview.ListFilter{PRURL: q.Get("pr_url"), State: q.Get("state")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			sreErrorCode(w, "limit must be an integer", "invalid_request",
				http.StatusBadRequest)
			return
		}
		f.Limit = n
	}
	reviews, err := svc.List(r.Context(), f)
	if err != nil {
		changeReviewError(w, err)
		return
	}
	views := make([]json.RawMessage, 0, len(reviews))
	for _, rv := range reviews {
		raw, err := changereview.RenderJSON(rv)
		if err != nil {
			changeReviewError(w, err)
			return
		}
		views = append(views, raw)
	}
	jsonResponse(w, map[string]any{"reviews": views})
}

func (h changeReviewHandlers) get(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	wait, ok := waitSeconds(w, r)
	if !ok {
		return
	}
	review, err := svc.Wait(r.Context(), r.PathValue("id"), wait)
	if err != nil {
		changeReviewError(w, err)
		return
	}
	writeChangeReview(w, review, nil)
}

// waitSeconds reads ?wait_seconds= (0..MaxWaitSeconds).
func waitSeconds(w http.ResponseWriter, r *http.Request) (time.Duration, bool) {
	raw := r.URL.Query().Get("wait_seconds")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > changereview.MaxWaitSeconds {
		sreErrorCode(w, fmt.Sprintf("wait_seconds must be 0-%d",
			changereview.MaxWaitSeconds), "invalid_request", http.StatusBadRequest)
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

func (h changeReviewHandlers) verdict(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.service(w, r)
	if !ok {
		return
	}
	var body struct {
		Verdict   string `json:"verdict"`
		Direction string `json:"direction"`
		Reason    string `json:"reason"`
	}
	if err := decodeStrictBody(r, &body); err != nil {
		sreErrorCode(w, `body must be {"verdict", "direction"?, "reason"?}`,
			"invalid_request", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	review, err := svc.Decide(r.Context(), id, changereview.DBAVerdict{Verdict: body.Verdict,
		Direction: body.Direction, Reason: body.Reason, Actor: changeReviewActor(r)})
	if errors.Is(err, changereview.ErrAlreadyDecided) {
		alreadyDecided(w, r.Context(), svc, id, err)
		return
	}
	if err != nil {
		changeReviewError(w, err)
		return
	}
	writeChangeReview(w, review, nil)
}

// alreadyDecided answers 409 naming who decided the review first.
func alreadyDecided(w http.ResponseWriter, ctx context.Context, svc *changereview.Service,
	id string, cause error) {
	body := map[string]string{"error": cause.Error(), "code": "already_decided"}
	if cur, err := svc.Get(ctx, id); err == nil {
		body["decided_by"], body["dba_verdict"] = cur.DBAActor, cur.DBAVerdict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(body)
}

func (h changeReviewHandlers) summary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	database := q.Get("database")
	if validateDatabaseParam(database) != nil || database == "all" {
		sreErrorCode(w, "invalid database name", "invalid_request", http.StatusBadRequest)
		return
	}
	days := defaultSummaryDays
	if raw := q.Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxSummaryDays {
			sreErrorCode(w, fmt.Sprintf("days must be 1-%d", maxSummaryDays),
				"invalid_request", http.StatusBadRequest)
			return
		}
		days = n
	}
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	s, err := h.reg.Summary(r.Context(), database, since)
	if errors.Is(err, changereview.ErrNotFound) {
		sreErrorCode(w, "database not found or Change Review is not running for it",
			"not_found", http.StatusNotFound)
		return
	}
	if err != nil {
		changeReviewError(w, err)
		return
	}
	jsonResponse(w, s)
}

// writeChangeReview answers the review's JSON view (no transcript, with
// its comment marker): 202 while it runs, else 200. created is set on a
// submit.
func writeChangeReview(w http.ResponseWriter, review changereview.Review, created *bool) {
	raw, err := changereview.RenderJSON(review)
	if err != nil {
		changeReviewError(w, err)
		return
	}
	if created != nil {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			changeReviewError(w, err)
			return
		}
		obj["created"], _ = json.Marshal(*created)
		if raw, err = json.Marshal(obj); err != nil {
			changeReviewError(w, err)
			return
		}
	}
	status := http.StatusOK
	if review.State == changereview.StateRunning {
		status = http.StatusAccepted
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// changeReviewError maps Change Review errors to distinguishable
// responses; store and internal failures are logged, not echoed.
func changeReviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, changereview.ErrInvalid):
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
	case errors.Is(err, changereview.ErrNotFound):
		sreErrorCode(w, "change review not found", "not_found", http.StatusNotFound)
	case errors.Is(err, changereview.ErrDisabled):
		sreErrorCode(w, "Change Review is disabled (change_review.enabled: false)",
			"review_disabled", http.StatusServiceUnavailable)
	case errors.Is(err, changereview.ErrBusy):
		w.Header().Set("Retry-After", busyRetryAfter)
		sreErrorCode(w, err.Error(), "review_busy", http.StatusTooManyRequests)
	case errors.Is(err, changereview.ErrAlreadyDecided):
		sreErrorCode(w, err.Error(), "already_decided", http.StatusConflict)
	case errors.Is(err, changereview.ErrUnavailable):
		slog.Warn("change review unavailable", "error", err)
		sreErrorCode(w, "Change Review is unavailable; try again", "review_unavailable",
			http.StatusServiceUnavailable)
	default:
		slog.Error("change review request failed", "error", err)
		sreErrorCode(w, "internal error", "internal_error", http.StatusInternalServerError)
	}
}
