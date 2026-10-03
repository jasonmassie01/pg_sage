package api

import (
	"encoding/json"
	"net/http"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Runbook and outcome writes. Each answers with the resulting runbook (or
// outcome); a new runbook answers 201.

func runbookCreateHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		var body struct {
			Definition json.RawMessage `json:"definition"`
		}
		err := decodeBody(r, &body)
		if err == nil {
			def, derr := definitionOf(body.Definition)
			var rb sre.Runbook
			if err = derr; err == nil {
				if rb, err = svc.CreateRunbook(r.Context(), def, actorOf(r)); err == nil {
					writeJSONStatus(w, http.StatusCreated, rb)
					return
				}
			}
		}
		runbookError(w, r, err)
	}
}

func runbookCompileHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		var body struct {
			Text string `json:"text"`
		}
		err := decodeBody(r, &body)
		if err == nil {
			var rb sre.Runbook
			if rb, err = svc.CompileRunbook(r.Context(), body.Text, actorOf(r)); err == nil {
				writeJSONStatus(w, http.StatusCreated, rb)
				return
			}
		}
		runbookError(w, r, err)
	}
}

func runbookReviseHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		var body struct {
			BaseVersion int             `json:"base_version"`
			Definition  json.RawMessage `json:"definition"`
		}
		id, err := runbookID(r)
		if err == nil {
			err = decodeBody(r, &body)
		}
		if err == nil {
			def, derr := definitionOf(body.Definition)
			var rb sre.Runbook
			if err = derr; err == nil {
				rb, err = svc.ReviseRunbook(r.Context(), id, body.BaseVersion, def,
					actorOf(r))
				if err == nil {
					jsonResponse(w, rb)
					return
				}
			}
		}
		runbookError(w, r, err)
	}
}

func runbookSignHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		var body struct {
			Version     int    `json:"version"`
			ContentHash string `json:"content_hash"`
		}
		id, err := runbookID(r)
		if err == nil {
			err = decodeBody(r, &body)
		}
		if err == nil {
			user := UserFromContext(r.Context())
			var rb sre.Runbook
			rb, err = svc.SignRunbook(r.Context(), id, body.Version, body.ContentHash,
				actorOf(r), user.Role)
			if err == nil {
				jsonResponse(w, rb)
				return
			}
		}
		runbookError(w, r, err)
	}
}

func runbookRetireHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		id, err := runbookID(r)
		if err == nil {
			var rb sre.Runbook
			if rb, err = svc.RetireRunbook(r.Context(), id, actorOf(r)); err == nil {
				jsonResponse(w, rb)
				return
			}
		}
		runbookError(w, r, err)
	}
}

func similarIncidentsHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		items, err := svc.Similar(r.Context(), sre.UUID(r.PathValue("id")))
		if err != nil {
			runbookError(w, r, err)
			return
		}
		jsonResponse(w, map[string]any{"label": sre.SimilarLabel, "items": items})
	}
}

// outcomeHandler records an investigation outcome. With the database's
// earned-autonomy ledger it also records the matching shadow review
// (outcomeWithReview), so the two records never disagree.
func outcomeHandler(mgr *fleet.DatabaseManager, ledgers *earned.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		var body struct {
			Verdict    string `json:"verdict"`
			ActualNode string `json:"actual_node"`
		}
		err := decodeBody(r, &body)
		if entry, found := ledgerOf(ledgers, r.PathValue("db")); err == nil && found {
			outcomeWithReview(w, r, entry.Service, svc, body.Verdict, body.ActualNode)
			return
		}
		if err == nil {
			var o sre.Outcome
			o, err = svc.RecordOutcome(r.Context(), sre.UUID(r.PathValue("id")),
				sre.OutcomeRequest{Verdict: body.Verdict, ActualNode: body.ActualNode,
					Actor: actorOf(r)})
			if err == nil {
				jsonResponse(w, o)
				return
			}
		}
		runbookError(w, r, err)
	}
}
