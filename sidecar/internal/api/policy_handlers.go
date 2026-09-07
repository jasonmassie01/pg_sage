package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/policy"
)

type policyService interface {
	Current(context.Context, policy.Scope) (policy.Policy, error)
	Propose(context.Context, policy.ProposalRequest) (policy.Proposal, error)
	Ratify(context.Context, policy.RatifyRequest) (policy.Policy, error)
	History(context.Context, policy.Scope, int) ([]policy.Policy, error)
}

func registerPolicyRoutes(mux *http.ServeMux, service policyService) {
	mux.HandleFunc("GET /api/v1/policy", authenticatedPolicy(
		func(w http.ResponseWriter, r *http.Request, _ *auth.User) {
			scope, ok := policyScope(w, r)
			if !ok {
				return
			}
			result, err := service.Current(r.Context(), scope)
			writePolicyResult(w, result, err, http.StatusOK)
		}))
	mux.HandleFunc("GET /api/v1/policy/history", authenticatedPolicy(
		func(w http.ResponseWriter, r *http.Request, _ *auth.User) {
			scope, ok := policyScope(w, r)
			if !ok {
				return
			}
			limit, ok := positiveQueryInt(w, r, "limit", 100)
			if !ok {
				return
			}
			result, err := service.History(r.Context(), scope, limit)
			writePolicyResult(w, map[string]any{"policies": result}, err, http.StatusOK)
		}))
	mux.HandleFunc("POST /api/v1/policy/proposals", authenticatedPolicy(
		func(w http.ResponseWriter, r *http.Request, user *auth.User) {
			if user.Role != auth.RoleAdmin && user.Role != auth.RoleOperator {
				jsonError(w, "insufficient permissions", http.StatusForbidden)
				return
			}
			request, ok := decodePolicyProposal(w, r, user.Email)
			if !ok {
				return
			}
			result, err := service.Propose(r.Context(), request)
			writePolicyResult(w, result, err, http.StatusCreated)
		}))
	mux.HandleFunc("POST /api/v1/policy/proposals/{id}/ratify", authenticatedPolicy(
		func(w http.ResponseWriter, r *http.Request, user *auth.User) {
			if user.Role != auth.RoleAdmin {
				jsonError(w, "insufficient permissions", http.StatusForbidden)
				return
			}
			request, ok := decodeRatification(w, r, user.Email)
			if !ok {
				return
			}
			result, err := service.Ratify(r.Context(), request)
			writePolicyResult(w, result, err, http.StatusOK)
		}))
}

type policyHandler func(http.ResponseWriter, *http.Request, *auth.User)

func authenticatedPolicy(next policyHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r, user)
	}
}

func policyScope(w http.ResponseWriter, r *http.Request) (policy.Scope, bool) {
	raw := r.URL.Query().Get("database_id")
	if raw == "" {
		return policy.Scope{}, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		jsonError(w, "database_id must be positive", http.StatusBadRequest)
		return policy.Scope{}, false
	}
	return policy.Scope{DatabaseID: &id}, true
}

func positiveQueryInt(
	w http.ResponseWriter, r *http.Request, name string, fallback int,
) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		jsonError(w, name+" must be positive", http.StatusBadRequest)
		return 0, false
	}
	return value, true
}

func decodePolicyProposal(
	w http.ResponseWriter, r *http.Request, actor string,
) (policy.ProposalRequest, bool) {
	var wire struct {
		DatabaseID      *int64               `json:"database_id"`
		ExpectedVersion int64                `json:"expected_version"`
		Profile         string               `json:"profile"`
		Document        json.RawMessage      `json:"document"`
		Preview         policy.ImpactPreview `json:"preview"`
		Actor           string               `json:"actor"`
	}
	if !decodePolicyJSON(w, r, &wire) || wire.ExpectedVersion <= 0 ||
		wire.Profile == "" || len(wire.Document) == 0 ||
		(wire.DatabaseID != nil && *wire.DatabaseID <= 0) {
		if wire.ExpectedVersion <= 0 {
			jsonError(w, "invalid proposal", http.StatusBadRequest)
		}
		return policy.ProposalRequest{}, false
	}
	return policy.ProposalRequest{Scope: policy.Scope{DatabaseID: wire.DatabaseID},
		ExpectedVersion: wire.ExpectedVersion, Profile: wire.Profile,
		Document: wire.Document, Actor: actor, Preview: wire.Preview}, true
}

func decodeRatification(
	w http.ResponseWriter, r *http.Request, actor string,
) (policy.RatifyRequest, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		jsonError(w, "invalid proposal id", http.StatusBadRequest)
		return policy.RatifyRequest{}, false
	}
	var wire struct {
		ExpectedVersion int64  `json:"expected_version"`
		Actor           string `json:"actor"`
	}
	if !decodePolicyJSON(w, r, &wire) || wire.ExpectedVersion <= 0 {
		if wire.ExpectedVersion <= 0 {
			jsonError(w, "invalid version", http.StatusBadRequest)
		}
		return policy.RatifyRequest{}, false
	}
	return policy.RatifyRequest{ProposalID: id, ExpectedVersion: wire.ExpectedVersion,
		Actor: actor}, true
}

func decodePolicyJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || strings.TrimSpace(rawJSON(target)) == "null" {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}

func rawJSON(target any) string {
	raw, _ := json.Marshal(target)
	return string(raw)
}

func writePolicyResult(w http.ResponseWriter, result any, err error, status int) {
	if err != nil {
		writePolicyError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if encodeErr := json.NewEncoder(w).Encode(result); encodeErr != nil {
		slog.Error("encode policy response", "error", encodeErr)
	}
}

func writePolicyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, policy.ErrInvalidDocument):
		jsonError(w, "invalid policy document", http.StatusBadRequest)
	case errors.Is(err, policy.ErrVersionConflict):
		jsonError(w, "policy version conflict", http.StatusConflict)
	case errors.Is(err, policy.ErrNotFound):
		jsonError(w, "policy not found", http.StatusNotFound)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		jsonError(w, "request canceled", http.StatusRequestTimeout)
	default:
		slog.Error("policy request failed", "error", err)
		jsonError(w, "policy request failed", http.StatusInternalServerError)
	}
}
