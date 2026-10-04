package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// One trust system (roadmap 1.2): GET /api/v1/trust serves the Trust
// page, the unified ledger of every database (or of ?database=) as
// database x family x class: level, effective level, evidence counts,
// last change and why, and the path to the next level. Every signed-in
// role reads it; promotions are approved through the existing autonomy
// routes (admin only).

const trustPath = "/api/v1/trust"

// trustMeaning says what the trust settings mean under the ledger.
const trustMeaning = "The trust ledger decides each self-initiated action class's " +
	"level per database from verified outcomes: L1 manual script, L2 one-click " +
	"approval, L3 unattended (irreversible classes never above L1). In short, " +
	"the ledger grants, the operator caps: trust.level and trust.tier3_safe / " +
	"tier3_moderate stay, permanently, the operator's ceiling and kill switch. " +
	"The trust ramp " +
	"(trust.ramp_safe_hours, ramp_moderate_hours) is the minimum observation before " +
	"pg_sage may propose a promotion, never a grant. Promotions need an admin's " +
	"approval; a regressed verdict, an operator rollback or a rejection demotes " +
	"the class one level."

func registerTrustRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	deps *AutonomyDeps) {
	if deps == nil || deps.Ledgers == nil {
		return
	}
	viewer := RequireRole("admin", "operator", "viewer")
	h := autonomyHandlers{deps: deps, mgr: mgr}
	mux.Handle("GET "+trustPath, viewer(http.HandlerFunc(h.trust)))
}

// trust serves one view per database.
func (h autonomyHandlers) trust(w http.ResponseWriter, r *http.Request) {
	names := h.deps.Ledgers.Databases()
	if name := r.URL.Query().Get("database"); name != "" {
		if len(name) > 200 {
			sreErrorCode(w, "database name is too long", "invalid_request",
				http.StatusBadRequest)
			return
		}
		if _, ok := h.deps.Ledgers.Lookup(name); !ok {
			sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
			return
		}
		names = []string{name}
	}
	views := make([]earned.TrustView, 0, len(names))
	for _, name := range names {
		v, ok, err := h.deps.Ledgers.TrustView(r.Context(), name)
		if err != nil {
			autonomyError(w, r, err)
			return
		}
		if ok { // else it left the fleet since it was listed
			views = append(views, v)
		}
	}
	jsonResponse(w, map[string]any{"databases": views,
		"enforced": h.deps.Ledgers.Enforced(), "meaning": trustMeaning})
}
