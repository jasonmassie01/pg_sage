package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// policySetService is the policy store's set surface (fleetpol-01a). A
// set's document changes only through Propose and the existing
// ratification route; these routes edit selectors, priority and
// requirements, list members and exceptions, and set an exception.
type policySetService interface {
	Sets(context.Context) ([]policy.SetDocument, error)
	PutSet(context.Context, policy.PutSetRequest) (policy.SetDocument, error)
	Members(context.Context, string) ([]policy.Resolution, error)
	Exceptions(context.Context) ([]policy.Exception, error)
	SetException(context.Context, policy.ExceptionRequest) (policy.Exception, error)
	Propose(context.Context, policy.ProposalRequest) (policy.Proposal, error)
}

// maxPolicySetBody bounds a set's JSON or YAML body.
const maxPolicySetBody = 1 << 20

// rubricDimensions are the review rubric's dimension ids a set's grade
// requirement may name.
func rubricDimensions() []string {
	dims := rubric.V1().Dimensions
	out := make([]string, 0, len(dims))
	for _, d := range dims {
		out = append(out, d.ID)
	}
	return out
}

func registerPolicySetRoutes(mux *http.ServeMux, service policySetService) {
	route(mux, "GET /api/v1/policy-sets", gateViewer, authenticatedPolicy(
		func(w http.ResponseWriter, r *http.Request, _ *auth.User) {
			sets, err := service.Sets(r.Context())
			writePolicySetResult(w, map[string]any{"sets": policySetViews(sets)}, err,
				http.StatusOK)
		}))
	route(mux, "PUT /api/v1/policy-sets/{name}", gateAdmin, adminPolicy(
		func(w http.ResponseWriter, r *http.Request, user *auth.User) {
			set, ok := decodePolicySetBody(w, r)
			if !ok {
				return
			}
			saved, err := service.PutSet(r.Context(), policy.PutSetRequest{Set: set,
				Actor: user.Email, Dimensions: rubricDimensions()})
			writePolicySetResult(w, policySetView(saved), err, http.StatusOK)
		}))
	route(mux, "GET /api/v1/policy-sets/{name}/members", gateViewer, authenticatedPolicy(
		func(w http.ResponseWriter, r *http.Request, _ *auth.User) {
			name := r.PathValue("name")
			members, err := service.Members(r.Context(), name)
			writePolicySetResult(w, map[string]any{"set": name, "members": members}, err,
				http.StatusOK)
		}))
	route(mux, "GET /api/v1/policy-exceptions", gateViewer, authenticatedPolicy(
		func(w http.ResponseWriter, r *http.Request, _ *auth.User) {
			list, err := service.Exceptions(r.Context())
			writePolicySetResult(w, map[string]any{"exceptions": list}, err, http.StatusOK)
		}))
	route(mux, "POST /api/v1/policy-exceptions", gateAdmin, adminPolicy(
		func(w http.ResponseWriter, r *http.Request, user *auth.User) {
			req, ok := decodeExceptionRequest(w, r, user.Email)
			if !ok {
				return
			}
			exc, err := service.SetException(r.Context(), req)
			writePolicySetResult(w, exc, err, http.StatusCreated)
		}))
	registerPolicySetFileRoutes(mux, service)
}

// adminPolicy is authenticatedPolicy for an admin: a set or an exception
// changes which document governs databases.
func adminPolicy(next policyHandler) http.HandlerFunc {
	return authenticatedPolicy(func(w http.ResponseWriter, r *http.Request, user *auth.User) {
		if user.Role != auth.RoleAdmin {
			jsonError(w, "insufficient permissions", http.StatusForbidden)
			return
		}
		next(w, r, user)
	})
}

// decodePolicySetBody reads a PUT body (priority, selectors, requirements)
// for the set named in the path and validates it.
func decodePolicySetBody(w http.ResponseWriter, r *http.Request) (policy.PolicySet, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPolicySetBody))
	if err != nil {
		jsonError(w, "policy set body is unreadable or over 1 MiB", http.StatusBadRequest)
		return policy.PolicySet{}, false
	}
	var wire struct {
		Priority     int                    `json:"priority"`
		Selectors    []policy.Selector      `json:"selectors"`
		Requirements policy.SetRequirements `json:"requirements"`
	}
	if err := policy.DecodeStrictJSON(raw, &wire); err != nil {
		jsonError(w, "invalid policy set: "+err.Error(), http.StatusBadRequest)
		return policy.PolicySet{}, false
	}
	set := policy.PolicySet{Name: r.PathValue("name"), Priority: wire.Priority,
		Selectors: wire.Selectors, Requirements: wire.Requirements}
	if err := policy.ValidateSet(set, rubricDimensions()); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return policy.PolicySet{}, false
	}
	return set, true
}

func decodeExceptionRequest(
	w http.ResponseWriter, r *http.Request, actor string,
) (policy.ExceptionRequest, bool) {
	var wire struct {
		DatabaseID int64      `json:"database_id"`
		Reason     string     `json:"reason"`
		ExpiresAt  *time.Time `json:"expires_at"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPolicySetBody)
	if !decodePolicyJSON(w, r, &wire) {
		return policy.ExceptionRequest{}, false
	}
	if wire.DatabaseID <= 0 || wire.Reason == "" {
		jsonError(w, "an exception needs a positive database_id and a reason",
			http.StatusBadRequest)
		return policy.ExceptionRequest{}, false
	}
	return policy.ExceptionRequest{DatabaseID: wire.DatabaseID, Actor: actor,
		Reason: wire.Reason, ExpiresAt: wire.ExpiresAt}, true
}

// policySetViewRow is a set with its document's version and hash, the
// names the gate's decisions record.
type policySetViewRow struct {
	policy.PolicySet
	PolicyID int64  `json:"policy_id,omitempty"`
	Version  int64  `json:"version,omitempty"`
	Profile  string `json:"profile,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	// ManagedBy is "config" for a set a GitOps file owns (cli-02a).
	ManagedBy string `json:"managed_by,omitempty"`
}

func policySetView(s policy.SetDocument) policySetViewRow {
	out := policySetViewRow{PolicySet: s.Set, ManagedBy: s.ManagedBy}
	if s.Policy != nil {
		out.PolicyID, out.Version, out.Profile = s.Policy.ID, s.Policy.Version, s.Policy.Profile
		if doc, err := policy.DocumentFromPolicy(*s.Policy); err == nil {
			out.SHA256 = doc.SHA256
		}
	}
	return out
}

func policySetViews(sets []policy.SetDocument) []policySetViewRow {
	out := make([]policySetViewRow, 0, len(sets))
	for _, s := range sets {
		out = append(out, policySetView(s))
	}
	return out
}

func writePolicySetResult(w http.ResponseWriter, result any, err error, status int) {
	switch {
	case err == nil:
		writePolicyResult(w, result, nil, status)
	case errors.Is(err, policy.ErrInvalidSet):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, policy.ErrSetConflict), errors.Is(err, policy.ErrExceptionExists):
		jsonError(w, err.Error(), http.StatusConflict)
	case errors.Is(err, policy.ErrNotFound):
		jsonError(w, err.Error(), http.StatusNotFound)
	default:
		writePolicyError(w, err)
	}
}
