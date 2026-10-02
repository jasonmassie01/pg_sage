package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Operator control of investigations (AI-SRE-SPEC §9, CHECK-24/25):
// operators start an investigation of a case, stop it (a resumable
// pause) and resume it. Stop and resume need the version the operator
// saw, as a JSON "version" or an If-Match header. Nothing here touches
// the monitored database or executes anything.

// maxOperatorBody bounds an operator request body.
const maxOperatorBody = 8 << 10

type startInvestigationRequest struct {
	CaseID         string `json:"case_id"`
	Kind           string `json:"kind"`
	Subject        string `json:"subject"`
	IdempotencyKey string `json:"idempotency_key"`
}

func operatorActor(r *http.Request) string {
	return fmt.Sprintf("user:%d", UserFromContext(r.Context()).ID)
}

func investigationStartHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		var req startInvestigationRequest
		if err := decodeOperatorBody(w, r, &req); err != nil {
			sreErrorResponse(w, r, err)
			return
		}
		inv, created, err := svc.Start(r.Context(), sre.Trigger{CaseID: req.CaseID,
			Kind: sre.TriggerKind(req.Kind), Subject: req.Subject,
			IdempotencyKey: req.IdempotencyKey, Actor: operatorActor(r)})
		if err != nil {
			sreOperatorError(w, r, err)
			return
		}
		if created {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(inv)
			return
		}
		jsonResponse(w, inv)
	}
}

func investigationTransitionHandler(mgr *fleet.DatabaseManager, resume bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc, ok := investigationService(w, mgr, r.PathValue("db"))
		if !ok {
			return
		}
		version, err := operatorVersion(w, r)
		if err != nil {
			sreOperatorError(w, r, err)
			return
		}
		id, actor := sre.UUID(r.PathValue("id")), operatorActor(r)
		var inv sre.Investigation
		if resume {
			inv, err = svc.Resume(r.Context(), id, version, actor)
		} else {
			inv, err = svc.Stop(r.Context(), id, version, actor)
		}
		if err != nil {
			sreOperatorError(w, r, err)
			return
		}
		jsonResponse(w, inv)
	}
}

// errVersionRequired is a stop or resume without the version the
// operator saw.
var errVersionRequired = errors.New("version required (JSON version or If-Match)")

// operatorVersion reads the If-Match header, else the JSON body's
// version.
func operatorVersion(w http.ResponseWriter, r *http.Request) (int64, error) {
	if h := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`); h != "" {
		v, err := strconv.ParseInt(h, 10, 64)
		if err != nil || v < 1 {
			return 0, fmt.Errorf("%w: If-Match must be a version number", sre.ErrInvalidRequest)
		}
		return v, nil
	}
	var body struct {
		Version *int64 `json:"version"`
	}
	if err := decodeOperatorBody(w, r, &body); err != nil {
		return 0, err
	}
	if body.Version == nil {
		return 0, errVersionRequired
	}
	return *body.Version, nil
}

// decodeOperatorBody decodes a bounded JSON body strictly; an empty body
// decodes as {}.
func decodeOperatorBody(w http.ResponseWriter, r *http.Request, out any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOperatorBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: body: %v", sre.ErrInvalidRequest, err)
	}
	return nil
}

// sreOperatorError adds the operator codes to the canonical ones.
func sreOperatorError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errVersionRequired):
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusPreconditionRequired)
	case errors.Is(err, sre.ErrVersionConflict):
		sreErrorCode(w, "the investigation changed; reload it", "version_conflict",
			http.StatusConflict)
	case errors.Is(err, sre.ErrInvalidTransition):
		sreErrorCode(w, err.Error(), "invalid_transition", http.StatusConflict)
	default:
		sreErrorResponse(w, r, err)
	}
}
