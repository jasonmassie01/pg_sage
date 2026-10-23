package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/broker"
)

// Agent activity (spec §8.3, G1-10). GET /api/v1/agents/{id}/activity
// (operator) → per database: live sessions, the statements
// pg_stat_statements attributes to the agent's roles, its agent_query
// audit rows, and whether attribution is complete; when it may have been
// dropped the audit rows are the record. ?database narrows to one
// database (and pages its audit rows with cursor); from and to default to
// the last 24 hours. No reader answers 503.

// AgentActivityReader reads a principal's activity.
type AgentActivityReader interface {
	AgentActivity(ctx context.Context, principalID, database string,
		req broker.ActivityRequest) ([]broker.Activity, error)
}

const defaultActivityLimit = 50

var principalIDPattern = regexp.MustCompile(`^agp_[a-z2-7]{20}$`)

func registerAgentActivityRoutes(mux *http.ServeMux, reader AgentActivityReader) {
	operatorUp := RequireRole("admin", "operator")
	h := agentActivityHandler{reader: reader}
	mux.Handle("GET /api/v1/agents/{id}/activity", operatorUp(http.HandlerFunc(h.get)))
}

type agentActivityHandler struct{ reader AgentActivityReader }

func (h agentActivityHandler) get(w http.ResponseWriter, r *http.Request) {
	if h.reader == nil {
		sreErrorCode(w, "agent activity needs agent governance (mode: meta or "+
			"agents.control_database)", "unavailable", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	database, req, msg := parseActivityQuery(r, time.Now())
	if !principalIDPattern.MatchString(id) {
		msg = "invalid agent id"
	}
	if msg != "" {
		sreErrorCode(w, msg, "invalid_arguments", http.StatusUnprocessableEntity)
		return
	}
	req.PrincipalID = id
	items, err := h.reader.AgentActivity(r.Context(), id, database, req)
	if err != nil {
		writeAgentActivityError(w, err)
		return
	}
	if items == nil {
		items = []broker.Activity{}
	}
	jsonResponse(w, map[string]any{"principal_id": id, "items": items})
}

// parseActivityQuery reads ?database&from&to&limit&cursor; msg is the
// first problem, "" when valid.
func parseActivityQuery(r *http.Request, now time.Time) (string, broker.ActivityRequest,
	string) {
	q := r.URL.Query()
	req := broker.ActivityRequest{From: now.Add(-24 * time.Hour), To: now,
		Limit: defaultActivityLimit, Cursor: q.Get("cursor")}
	database := q.Get("database")
	if database != "" && validateDatabaseParam(database) != nil {
		return "", req, "invalid database name"
	}
	for key, target := range map[string]*time.Time{"from": &req.From, "to": &req.To} {
		if v := q.Get(key); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return "", req, key + " must be an RFC 3339 time"
			}
			*target = t
		}
	}
	if !req.From.Before(req.To) {
		return "", req, "from must be before to"
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > broker.MaxActivityLimit {
			return "", req, "limit must be 1-200"
		}
		req.Limit = n
	}
	return database, req, ""
}

func writeAgentActivityError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agentguard.ErrNotFound):
		sreErrorCode(w, "agent not found", "not_found", http.StatusNotFound)
	case errors.Is(err, broker.ErrUnknownDatabase):
		sreErrorCode(w, "unknown database", "unknown_database", http.StatusNotFound)
	case errors.Is(err, broker.ErrInvalid):
		sreErrorCode(w, err.Error(), "invalid_arguments", http.StatusUnprocessableEntity)
	case errors.Is(err, broker.ErrUnavailable):
		sreErrorCode(w, "agent governance is unavailable", "unavailable",
			http.StatusServiceUnavailable)
	default:
		sreErrorCode(w, "internal error", "internal_error", http.StatusInternalServerError)
	}
}
