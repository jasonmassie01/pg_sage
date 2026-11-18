package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/policy"
)

// The file form of a set and its parser are policy.SetFile and
// policy.DecodeSetFile (fleetpol-01a), shared with the GitOps directory
// (cli-02a). Importing saves the set's selectors, priority and
// requirements and, when the document differs, files a proposal against
// the file's version: the document still needs ratification.

func registerPolicySetFileRoutes(mux *http.ServeMux, service policySetService) {
	route(mux, "GET /api/v1/policy-sets/{name}/export", gateViewer, authenticatedPolicy(
		func(w http.ResponseWriter, r *http.Request, _ *auth.User) {
			set, err := findPolicySet(r, service, r.PathValue("name"))
			if err != nil {
				writePolicySetResult(w, nil, err, http.StatusOK)
				return
			}
			out, err := encodePolicySetFile(set)
			if err != nil {
				writePolicySetResult(w, nil, err, http.StatusOK)
				return
			}
			w.Header().Set("Content-Type", "application/yaml")
			if _, err := w.Write(out); err != nil {
				slog.Error("write policy set export", "set", set.Set.Name, "error", err)
			}
		}))
	route(mux, "POST /api/v1/policy-sets/import", gateAdmin, adminPolicy(
		func(w http.ResponseWriter, r *http.Request, user *auth.User) {
			file, ok := decodePolicySetFile(w, r)
			if !ok {
				return
			}
			result, err := importPolicySet(r, service, file, user.Email)
			writePolicySetResult(w, result, err, http.StatusOK)
		}))
}

func findPolicySet(
	r *http.Request, service policySetService, name string,
) (policy.SetDocument, error) {
	sets, err := service.Sets(r.Context())
	if err != nil {
		return policy.SetDocument{}, err
	}
	for _, s := range sets {
		if s.Set.Name == name {
			return s, nil
		}
	}
	return policy.SetDocument{}, fmt.Errorf("%w: policy set %q", policy.ErrNotFound, name)
}

func encodePolicySetFile(set policy.SetDocument) ([]byte, error) {
	return policy.EncodeSetFile(set)
}

// decodePolicySetFile reads a YAML file (unknown keys refused, at most
// 1 MiB) and validates its set.
func decodePolicySetFile(w http.ResponseWriter, r *http.Request) (policy.SetFile, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPolicySetBody))
	if err != nil {
		jsonError(w, "policy set file is unreadable or over 1 MiB", http.StatusBadRequest)
		return policy.SetFile{}, false
	}
	file, err := policy.DecodeSetFile(raw, rubricDimensions())
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return policy.SetFile{}, false
	}
	return file, true
}

// importPolicySet saves the file's set and, when its document differs
// from the set's current one, proposes it against the file's version.
func importPolicySet(
	r *http.Request, service policySetService, file policy.SetFile, actor string,
) (map[string]any, error) {
	saved, err := service.PutSet(r.Context(), policy.PutSetRequest{Set: file.Set(),
		Actor: actor, Dimensions: rubricDimensions()})
	if err != nil {
		return nil, err
	}
	result := map[string]any{"set": policySetView(saved)}
	if file.Document == nil {
		return result, nil
	}
	current, err := findPolicySet(r, service, file.Name)
	if err != nil {
		return nil, err
	}
	if same, err := sameSetDocument(current.Policy, file.Document); err != nil || same {
		return result, err
	}
	proposal, err := service.Propose(r.Context(), policy.ProposalRequest{
		Scope: policy.Scope{SetName: file.Name}, ExpectedVersion: file.Document.Version,
		Profile: file.Document.Profile, Document: file.Document.Body, Actor: actor})
	if err != nil {
		return nil, err
	}
	result["proposal_id"] = proposal.ID
	return result, nil
}

func sameSetDocument(current *policy.Policy, file *policy.SetFileDocument) (bool, error) {
	if current == nil || current.Profile != file.Profile {
		return false, nil
	}
	diff, err := policy.DocumentDiff(current.Document, file.Body)
	if err != nil {
		return false, fmt.Errorf("%w: document: %v", policy.ErrInvalidSet, err)
	}
	return len(diff) == 0, nil
}
