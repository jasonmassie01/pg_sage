package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// fleetpol-01a: the policy-set routes over a fake store. The store's own
// behaviour on a real server is policy.TestPolicySetDB_*.

type fakePolicySets struct {
	sets       []policy.SetDocument
	err        error
	put        []policy.PutSetRequest
	members    []policy.Resolution
	membersOf  string
	exceptions []policy.Exception
	excReq     []policy.ExceptionRequest
	proposals  []policy.ProposalRequest
}

func (f *fakePolicySets) Sets(context.Context) ([]policy.SetDocument, error) {
	return f.sets, f.err
}

func (f *fakePolicySets) PutSet(
	_ context.Context, r policy.PutSetRequest,
) (policy.SetDocument, error) {
	f.put = append(f.put, r)
	if f.err != nil {
		return policy.SetDocument{}, f.err
	}
	return policy.SetDocument{Set: r.Set, Policy: &policy.Policy{ID: 5, Version: 1}}, nil
}

func (f *fakePolicySets) Members(_ context.Context, name string) ([]policy.Resolution, error) {
	f.membersOf = name
	return f.members, f.err
}

func (f *fakePolicySets) Exceptions(context.Context) ([]policy.Exception, error) {
	return f.exceptions, f.err
}

func (f *fakePolicySets) SetException(
	_ context.Context, r policy.ExceptionRequest,
) (policy.Exception, error) {
	f.excReq = append(f.excReq, r)
	return policy.Exception{DatabaseID: r.DatabaseID, SetName: "prod", SetBy: r.Actor}, f.err
}

func (f *fakePolicySets) Propose(
	_ context.Context, r policy.ProposalRequest,
) (policy.Proposal, error) {
	f.proposals = append(f.proposals, r)
	return policy.Proposal{ID: 77, BaseVersion: r.ExpectedVersion}, f.err
}

func policySetHandler(f *fakePolicySets) http.Handler {
	mux := http.NewServeMux()
	registerPolicySetRoutes(mux, f)
	return mux
}

func TestPolicySetPut_AdminOnlyAndUsesPathNameAndActor(t *testing.T) {
	body := `{"priority":10,"selectors":[{"tags":{"tier":"prod"}}],
		"requirements":{"grades":[{"dimension":"recoverability","min_letter":"C",
		"within_hours":24}]}}`
	for _, u := range []userFixture{viewerFixture, operatorFixture} {
		f := &fakePolicySets{}
		resp := requestPolicy(policySetHandler(f), http.MethodPut,
			"/api/v1/policy-sets/prod", body, u.user())
		require.Equal(t, http.StatusForbidden, resp.Code)
		require.Len(t, f.put, 0)
	}
	f := &fakePolicySets{}
	resp := requestPolicy(policySetHandler(f), http.MethodPut, "/api/v1/policy-sets/prod",
		body, adminFixture.user())
	require.Equal(t, http.StatusOK, resp.Code)
	require.Len(t, f.put, 1)
	require.Equal(t, "prod", f.put[0].Set.Name)
	require.Equal(t, 10, f.put[0].Set.Priority)
	require.Equal(t, "admin@test.com", f.put[0].Actor)
	if !slices.Contains(f.put[0].Dimensions, "recoverability") {
		t.Fatalf("PUT validated against dimensions %v, want review-01's", f.put[0].Dimensions)
	}
}

func TestPolicySetPut_RefusesUnknownKeyDimensionAndTie(t *testing.T) {
	cases := []struct {
		name, body string
		err        error
		status     int
		calls      int
	}{
		{"unknown selector key", `{"priority":5,"selectors":[{"colour":"red"}]}`, nil,
			http.StatusBadRequest, 0},
		{"unknown dimension", `{"priority":5,"selectors":[{"all":true}],"requirements":
			{"grades":[{"dimension":"vibes","min_letter":"C","within_hours":1}]}}`, nil,
			http.StatusBadRequest, 0},
		{"not json", `{`, nil, http.StatusBadRequest, 0},
		{"tie", `{"priority":10,"selectors":[{"tags":{"team":"a"}}]}`,
			fmt.Errorf("%w: database 1", policy.ErrSetConflict), http.StatusConflict, 1},
		{"store down", `{"priority":10,"selectors":[{"tags":{"team":"a"}}]}`,
			errors.New("connection refused"), http.StatusInternalServerError, 1},
	}
	for _, tc := range cases {
		f := &fakePolicySets{err: tc.err}
		resp := requestPolicy(policySetHandler(f), http.MethodPut, "/api/v1/policy-sets/team-a",
			tc.body, adminFixture.user())
		if resp.Code != tc.status || len(f.put) != tc.calls {
			t.Errorf("%s: status %d calls %d, want %d and %d (%s)", tc.name, resp.Code,
				len(f.put), tc.status, tc.calls, resp.Body.String())
		}
	}
}

func TestPolicySetGet_ListsSetsMembersAndExceptions(t *testing.T) {
	expires := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	f := &fakePolicySets{
		sets: []policy.SetDocument{{Set: policy.PolicySet{Name: "prod", Priority: 10},
			Policy: &policy.Policy{ID: 3, Version: 2}}},
		members: []policy.Resolution{{Member: policy.Member{DatabaseID: 4, Name: "orders"},
			Set: "prod", Policy: policy.Policy{ID: 3, Version: 2}}},
		exceptions: []policy.Exception{{DatabaseID: 9, SetName: "default", SetBy: "dba",
			Reason: "batch", ExpiresAt: &expires,
			Diff: []policy.FieldDiff{{Field: "serialize_mode"}}}},
	}
	h := policySetHandler(f)
	resp := requestPolicy(h, http.MethodGet, "/api/v1/policy-sets", "", viewerFixture.user())
	require.Equal(t, http.StatusOK, resp.Code)
	var sets struct {
		Sets []struct {
			Name    string `json:"name"`
			Version int64  `json:"version"`
		} `json:"sets"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &sets))
	require.Len(t, sets.Sets, 1)
	require.Equal(t, "prod", sets.Sets[0].Name)
	require.Equal(t, int64(2), sets.Sets[0].Version)

	resp = requestPolicy(h, http.MethodGet, "/api/v1/policy-sets/prod/members", "",
		viewerFixture.user())
	require.Equal(t, http.StatusOK, resp.Code)
	require.Equal(t, "prod", f.membersOf)
	require.True(t, strings.Contains(resp.Body.String(), `"orders"`))

	resp = requestPolicy(h, http.MethodGet, "/api/v1/policy-exceptions", "",
		viewerFixture.user())
	require.Equal(t, http.StatusOK, resp.Code)
	var exc struct {
		Exceptions []policy.Exception `json:"exceptions"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &exc))
	require.Len(t, exc.Exceptions, 1)
	require.Equal(t, "serialize_mode", exc.Exceptions[0].Diff[0].Field)
	require.True(t, exc.Exceptions[0].ExpiresAt.Equal(expires))

	f.err = policy.ErrNotFound
	resp = requestPolicy(h, http.MethodGet, "/api/v1/policy-sets/nope/members", "",
		viewerFixture.user())
	require.Equal(t, http.StatusNotFound, resp.Code)
	resp = requestPolicy(h, http.MethodGet, "/api/v1/policy-sets", "", nil)
	require.Equal(t, http.StatusUnauthorized, resp.Code)
}

func TestPolicyExceptionPost_AdminOnlyWithReason(t *testing.T) {
	f := &fakePolicySets{}
	h := policySetHandler(f)
	body := `{"database_id":9,"reason":"nightly batch","expires_at":"2030-01-01T00:00:00Z"}`
	resp := requestPolicy(h, http.MethodPost, "/api/v1/policy-exceptions", body,
		operatorFixture.user())
	require.Equal(t, http.StatusForbidden, resp.Code)
	require.Len(t, f.excReq, 0)
	resp = requestPolicy(h, http.MethodPost, "/api/v1/policy-exceptions", body,
		adminFixture.user())
	require.Equal(t, http.StatusCreated, resp.Code)
	require.Len(t, f.excReq, 1)
	require.Equal(t, int64(9), f.excReq[0].DatabaseID)
	require.Equal(t, "admin@test.com", f.excReq[0].Actor)
	require.Equal(t, "nightly batch", f.excReq[0].Reason)
	for _, bad := range []string{`{"database_id":0,"reason":"r"}`, `{"database_id":9}`,
		`{"database_id":9,"reason":"r","extra":1}`} {
		f.excReq = nil
		resp = requestPolicy(h, http.MethodPost, "/api/v1/policy-exceptions", bad,
			adminFixture.user())
		require.Equal(t, http.StatusBadRequest, resp.Code)
		require.Len(t, f.excReq, 0)
	}
	f.err = policy.ErrExceptionExists
	resp = requestPolicy(h, http.MethodPost, "/api/v1/policy-exceptions", body,
		adminFixture.user())
	require.Equal(t, http.StatusConflict, resp.Code)
}

const exportDoc = `{"allowed_change_classes":["analyze"],"serialize_mode":"park"}`

func TestPolicySetExportImport_RoundTripsAndProposesDocumentChanges(t *testing.T) {
	f := &fakePolicySets{sets: []policy.SetDocument{{
		Set: policy.PolicySet{Name: "prod", Priority: 10,
			Selectors: []policy.Selector{{Tags: map[string]string{"tier": "prod"}}}},
		Policy: &policy.Policy{ID: 3, Version: 2, Profile: "staffed",
			Document: json.RawMessage(exportDoc)}}}}
	h := policySetHandler(f)
	resp := requestPolicy(h, http.MethodGet, "/api/v1/policy-sets/prod/export", "",
		viewerFixture.user())
	require.Equal(t, http.StatusOK, resp.Code)
	require.True(t, strings.HasPrefix(resp.Header().Get("Content-Type"), "application/yaml"))
	exported := resp.Body.String()
	for _, want := range []string{"name: prod", "priority: 10", "tier: prod",
		"allowed_change_classes", "version: 2"} {
		if !strings.Contains(exported, want) {
			t.Fatalf("export lacks %q:\n%s", want, exported)
		}
	}

	resp = importYAML(h, exported, adminFixture.user())
	require.Equal(t, http.StatusOK, resp.Code)
	require.Len(t, f.put, 1)
	require.Len(t, f.proposals, 0) // the same document files no proposal

	changed := addListItem(exported, "- analyze", "- vacuum")
	require.True(t, changed != exported)
	resp = importYAML(h, changed, adminFixture.user())
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	require.Len(t, f.proposals, 1)
	require.Equal(t, "prod", f.proposals[0].Scope.SetName)
	require.Equal(t, int64(2), f.proposals[0].ExpectedVersion)
	require.Equal(t, "admin@test.com", f.proposals[0].Actor)
	require.True(t, strings.Contains(resp.Body.String(), `"proposal_id":77`))

	resp = importYAML(h, "name: [", adminFixture.user())
	require.Equal(t, http.StatusBadRequest, resp.Code)
	resp = importYAML(h, exported, operatorFixture.user())
	require.Equal(t, http.StatusForbidden, resp.Code)
	resp = requestPolicy(h, http.MethodGet, "/api/v1/policy-sets/nope/export", "",
		viewerFixture.user())
	require.Equal(t, http.StatusNotFound, resp.Code)
}

// addListItem inserts item after the line holding marker, at its indent.
func addListItem(text, marker, item string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == marker {
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			out := append(append(slices.Clone(lines[:i+1]), indent+item), lines[i+1:]...)
			return strings.Join(out, "\n")
		}
	}
	return text
}

func importYAML(h http.Handler, body string, user *auth.User) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/policy-sets/import",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/yaml")
	req = withUser(req, user)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPolicyProposal_SetNameScopesTheProposalToTheSet(t *testing.T) {
	service := &fakePolicyService{proposeResult: policy.Proposal{ID: 4}}
	handler := policyTestHandler(service)
	doc := `"document":{"unknown_classification":"fail_closed"}`
	resp := requestPolicy(handler, http.MethodPost, "/api/v1/policy/proposals",
		`{"set_name":"prod","expected_version":2,"profile":"staffed",`+doc+`}`,
		testOperatorUser())
	require.Equal(t, http.StatusCreated, resp.Code)
	require.Equal(t, "prod", service.proposeRequest.Scope.SetName)
	require.Nil(t, service.proposeRequest.Scope.DatabaseID)
	resp = requestPolicy(handler, http.MethodPost, "/api/v1/policy/proposals",
		`{"set_name":"prod","database_id":3,"expected_version":2,"profile":"staffed",`+
			doc+`}`, testOperatorUser())
	require.Equal(t, http.StatusBadRequest, resp.Code)
	require.Equal(t, 1, service.proposeCalls)
	resp = requestPolicy(handler, http.MethodGet, "/api/v1/policy?set_name=prod", "",
		testViewerUser())
	require.Equal(t, http.StatusOK, resp.Code)
	require.Equal(t, "prod", service.currentScope.SetName)
	resp = requestPolicy(handler, http.MethodGet, "/api/v1/policy?set_name=prod&database_id=1",
		"", testViewerUser())
	require.Equal(t, http.StatusBadRequest, resp.Code)
}
