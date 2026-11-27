package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/earned"
)

// revalidate is an admin's re-approval of a self-initiated level capped
// at L2 because the monitored server now runs another PostgreSQL major
// than the one it was granted on (earned-07). Like a promotion, it is the
// human trust step, so only an admin may take it; the admin is recorded.
func (h autonomyHandlers) revalidate(w http.ResponseWriter, r *http.Request) {
	e, _, ok := h.ledger(w, r.URL.Query().Get("database"))
	var body struct {
		Family string `json:"family"`
		Class  string `json:"class"`
		Note   string `json:"note"`
	}
	if !ok || !decodeAutonomyBody(w, r, &body) {
		return
	}
	if e.Limiter == nil {
		sreErrorCode(w, "the database has no autonomy limiter", "not_configured",
			http.StatusConflict)
		return
	}
	st, err := e.Limiter.Revalidate(r.Context(), earned.Family(body.Family),
		earned.ActionClass(body.Class), autonomyActor(UserFromContext(r.Context())), body.Note)
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	jsonResponse(w, map[string]any{"state": st})
}
