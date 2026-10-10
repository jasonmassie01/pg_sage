package decommission

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Handlers serve the §12 inventory and acknowledgement (AGENTDB-SPEC §8.3).
// The API router mounts them admin-only on the control pool.
type Handlers struct {
	pool  *pgxpool.Pool
	actor func(*http.Request) string
	logf  func(format string, args ...any)
}

// NewHandlers builds the handlers. actor names the signed-in human; logf
// receives store failures, which are never echoed to the caller.
func NewHandlers(pool *pgxpool.Pool, actor func(*http.Request) string,
	logf func(format string, args ...any)) Handlers {
	return Handlers{pool: pool, actor: actor, logf: logf}
}

// Inventory answers GET InventoryPath with the inventory as JSON.
func (h Handlers) Inventory(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "no control database"})
		return
	}
	inv, err := Build(r.Context(), h.pool)
	if err != nil {
		h.logf("decommission inventory failed: %v", err)
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "inventory unavailable; see the sidecar log"})
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

// Ack answers POST AckPath: {acknowledged_resources: [id], exported: true}.
func (h Handlers) Ack(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "no control database"})
		return
	}
	actor := h.actor(r)
	if actor == "" {
		writeJSON(w, http.StatusUnauthorized,
			map[string]any{"error": "authentication required"})
		return
	}
	var req AckRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	res, err := Acknowledge(r.Context(), h.pool, req, actor, SourceAPI)
	var unknownErr *UnknownResourcesError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res)
	case errors.As(err, &unknownErr):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "unknown resources", "unknown_resources": unknownErr.IDs})
	case errors.Is(err, ErrNotExported), errors.Is(err, ErrNoResources):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	case errors.Is(err, ErrAckTableMissing):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
	default:
		h.logf("decommission acknowledgement failed: %v", err)
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "acknowledgement failed; see the sidecar log"})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
