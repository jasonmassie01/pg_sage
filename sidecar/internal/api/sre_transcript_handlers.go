package api

import (
	"errors"
	"net/http"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
)

// The tool-calling investigator's transcript (roadmap 2.1):
// GET /api/v1/databases/{db}/investigations/{id}/transcript serves the
// plan, each tool call with its redacted result and digest, the cited
// claims and the outcome with its authority. Every signed-in role reads
// it with identifiers as keyed hashes (the replay export's default-deny
// redaction); ?keep_identifiers=true is for operators and admins.

func registerTranscriptRoute(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	mux.Handle("GET "+sreInvestigationsPath+"/{id}/transcript",
		RequireRole("admin", "operator", "viewer")(investigationTranscriptHandler(mgr)))
}

func investigationTranscriptHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keep, ok := keepIdentifiers(r.URL.Query().Get("keep_identifiers"))
		if !ok {
			sreErrorCode(w, "keep_identifiers must be true or false", "invalid_request",
				http.StatusBadRequest)
			return
		}
		if u := UserFromContext(r.Context()); keep && (u == nil ||
			u.Role != "admin" && u.Role != "operator") {
			sreErrorCode(w, "keeping identifiers needs the operator or admin role",
				"forbidden", http.StatusForbidden)
			return
		}
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		view, err := svc.Transcript(r.Context(), sre.UUID(r.PathValue("id")),
			sre.TranscriptOptions{KeepIdentifiers: keep})
		switch {
		case errors.Is(err, sre.ErrNoTranscript):
			sreErrorCode(w, "this investigation did not run the investigator",
				"no_transcript", http.StatusNotFound)
		case err != nil:
			sreErrorResponse(w, r, err)
		default:
			jsonResponse(w, view)
		}
	}
}
