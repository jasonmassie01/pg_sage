package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"gopkg.in/yaml.v3"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/policy"
)

// policySetFile is a set's file form (YAML): its selectors, priority and
// requirements, and its document with the version it was exported at.
// Importing saves the first three and, when the document differs, files a
// proposal against that version: the document still needs ratification.
type policySetFile struct {
	Name         string                 `json:"name"`
	Priority     int                    `json:"priority"`
	Selectors    []policy.Selector      `json:"selectors"`
	Requirements policy.SetRequirements `json:"requirements"`
	Document     *policySetFileDocument `json:"document,omitempty"`
}

type policySetFileDocument struct {
	Version int64           `json:"version"`
	Profile string          `json:"profile"`
	Body    json.RawMessage `json:"body"`
}

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

// encodePolicySetFile writes the file form through JSON, so YAML keys are
// the API's JSON names.
func encodePolicySetFile(set policy.SetDocument) ([]byte, error) {
	file := policySetFile{Name: set.Set.Name, Priority: set.Set.Priority,
		Selectors: set.Set.Selectors, Requirements: set.Set.Requirements}
	if set.Policy != nil {
		file.Document = &policySetFileDocument{Version: set.Policy.Version,
			Profile: set.Policy.Profile, Body: set.Policy.Document}
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("encode policy set %s: %w", set.Set.Name, err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("encode policy set %s: %w", set.Set.Name, err)
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(tree); err != nil {
		return nil, fmt.Errorf("encode policy set %s as YAML: %w", set.Set.Name, err)
	}
	return out.Bytes(), encoder.Close()
}

// decodePolicySetFile reads a YAML file (unknown keys refused, at most
// 1 MiB) and validates its set.
func decodePolicySetFile(w http.ResponseWriter, r *http.Request) (policySetFile, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPolicySetBody))
	if err != nil {
		jsonError(w, "policy set file is unreadable or over 1 MiB", http.StatusBadRequest)
		return policySetFile{}, false
	}
	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		jsonError(w, "policy set file is not YAML: "+err.Error(), http.StatusBadRequest)
		return policySetFile{}, false
	}
	asJSON, err := json.Marshal(tree)
	var file policySetFile
	if err == nil {
		err = policy.DecodeStrictJSON(asJSON, &file)
	}
	if err != nil {
		jsonError(w, "invalid policy set file: "+err.Error(), http.StatusBadRequest)
		return policySetFile{}, false
	}
	set := policy.PolicySet{Name: file.Name, Priority: file.Priority,
		Selectors: file.Selectors, Requirements: file.Requirements}
	if err := policy.ValidateSet(set, rubricDimensions()); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return policySetFile{}, false
	}
	return file, true
}

// importPolicySet saves the file's set and, when its document differs
// from the set's current one, proposes it against the file's version.
func importPolicySet(
	r *http.Request, service policySetService, file policySetFile, actor string,
) (map[string]any, error) {
	saved, err := service.PutSet(r.Context(), policy.PutSetRequest{Set: policy.PolicySet{
		Name: file.Name, Priority: file.Priority, Selectors: file.Selectors,
		Requirements: file.Requirements}, Actor: actor, Dimensions: rubricDimensions()})
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

func sameSetDocument(current *policy.Policy, file *policySetFileDocument) (bool, error) {
	if current == nil || current.Profile != file.Profile {
		return false, nil
	}
	diff, err := policy.DocumentDiff(current.Document, file.Body)
	if err != nil {
		return false, fmt.Errorf("%w: document: %v", policy.ErrInvalidSet, err)
	}
	return len(diff) == 0, nil
}
