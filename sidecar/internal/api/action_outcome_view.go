package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/verify"
)

// actionOutcomeColumnSQL is an executed action's predicted-vs-observed
// verdict (Phase 1.3), one primary-key read of sage.action_outcome per
// action row (the row's table or alias is action_log).
const actionOutcomeColumnSQL = `
 (SELECT jsonb_build_object('class', o.action_class, 'verdict', o.verdict,
   'tolerance', o.tolerance, 'predicted', o.predicted, 'observed', o.observed,
   'evidence', o.evidence, 'reason', o.reason, 'window_start', o.window_start,
   'window_end', o.window_end, 'decided_at', o.decided_at)
   FROM sage.action_outcome o WHERE o.action_log_id = action_log.id)
   AS verification_outcome`

// annotateOutcome attaches the decoded verdict; an action without one
// gets null.
func annotateOutcome(a map[string]any, raw []byte) {
	var outcome map[string]any
	if len(raw) > 0 && json.Unmarshal(raw, &outcome) != nil {
		outcome = nil
	}
	if outcome == nil {
		a["verification_outcome"] = nil
		return
	}
	a["verification_outcome"] = outcome
}

// actionOutcomesHandler serves the outcome ledger the trust system reads:
// GET /api/v1/action-outcomes?database=&class=&verdict=&since=&limit=,
// newest action first.
func actionOutcomesHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		filter, err := parseOutcomeFilter(r)
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
		outcomes, err := verify.NewOutcomeStore(selected.pool).List(r.Context(), filter)
		if errors.Is(err, verify.ErrInvalidOutcome) {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			internalError(w, r, "list action outcomes", err)
			return
		}
		jsonResponse(w, map[string]any{"database": selected.name, "outcomes": outcomes})
	}
}

func parseOutcomeFilter(r *http.Request) (verify.OutcomeFilter, error) {
	q := r.URL.Query()
	f := verify.OutcomeFilter{Class: q.Get("class"), Verdict: q.Get("verdict"), Limit: 100}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			return f, errors.New("limit must be an integer 1-1000")
		}
		f.Limit = n
	}
	if raw := q.Get("since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return f, errors.New("since must be an RFC 3339 time")
		}
		f.Since = since
	}
	return f, nil
}
