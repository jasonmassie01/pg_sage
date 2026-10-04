package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/specialist"
)

// The Postgres-specialist contract (roadmap phase 3) is served under
// specialist.BasePath by its own handler, which authenticates MCP tokens
// only: like the MCP endpoint, a session cookie never authenticates it
// (agents send bearer tokens; a browser would send the cookie cross-site).
// People read the request audit (which agent asked what) with a session.

// SpecialistAuditReader lists the newest specialist-contract requests.
type SpecialistAuditReader interface {
	Recent(ctx context.Context, limit int) ([]specialist.Record, error)
}

const specialistPrefix = specialist.BasePath + "/"

// isSpecialistRequest reports a request the session middleware leaves to
// the contract's own bearer check.
func isSpecialistRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, specialistPrefix)
}

func registerSpecialistRoutes(mux *http.ServeMux, rt RuntimeDeps) {
	if rt.Specialist != nil {
		mux.Handle(specialistPrefix, rt.Specialist)
	}
	if rt.SpecialistAudit != nil {
		mux.Handle("GET /api/v1/specialist-requests", RequireRole("admin", "operator")(
			specialistAuditHandler(rt.SpecialistAudit)))
	}
}

// specialistAuditView is one audit row as people see it: who asked what
// and what pg_sage answered; never the caller's free text.
type specialistAuditView struct {
	ID              string                  `json:"id"`
	Kind            string                  `json:"kind"`
	IdentityName    string                  `json:"identity_name"`
	Actor           string                  `json:"actor"`
	Transport       string                  `json:"transport"`
	Database        string                  `json:"database"`
	InvestigationID string                  `json:"investigation_id,omitempty"`
	Created         bool                    `json:"created"`
	Match           string                  `json:"match,omitempty"`
	ExternalRef     *specialist.ExternalRef `json:"external_ref,omitempty"`
	RemediationID   string                  `json:"remediation_id,omitempty"`
	Verdict         string                  `json:"verdict,omitempty"`
	Outbound        string                  `json:"outbound"`
	OutboundError   string                  `json:"outbound_error,omitempty"`
	CreatedAt       time.Time               `json:"created_at"`
}

func specialistAuditHandler(audit SpecialistAuditReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 500 {
				jsonError(w, "limit must be 1-500", http.StatusBadRequest)
				return
			}
			limit = n
		}
		recs, err := audit.Recent(r.Context(), limit)
		if err != nil {
			slog.Error("specialist request audit failed", "err", err)
			jsonError(w, "request audit unavailable", http.StatusServiceUnavailable)
			return
		}
		items := make([]specialistAuditView, 0, len(recs))
		for _, rec := range recs {
			items = append(items, specialistAuditView{ID: rec.ID, Kind: rec.Kind,
				IdentityName: rec.IdentityName, Actor: rec.Actor, Transport: rec.Transport,
				Database: rec.Database, InvestigationID: rec.InvestigationID,
				Created: rec.Created, Match: rec.Match, ExternalRef: rec.ExternalRef,
				RemediationID: rec.RemediationID, Verdict: rec.Verdict,
				Outbound: rec.Outbound, OutboundError: rec.OutboundError,
				CreatedAt: rec.CreatedAt})
		}
		jsonResponse(w, map[string]any{"items": items})
	}
}
